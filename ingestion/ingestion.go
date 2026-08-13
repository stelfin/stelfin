// Package ingestion turns what happened on Stellar into ledger entries.
//
// Stellar is the authority on balances; this package is how the ledger learns
// what the chain decided. It never originates money movement, only records it.
//
// Delivery is at-least-once. The cursor advances only after the corresponding
// ledger transaction has committed, so a crash in between replays an operation
// rather than dropping it, and replays are harmless because each operation
// posts under an idempotency key derived from its Horizon id. Advancing the
// cursor first would be at-most-once and would silently lose payments, which is
// the one failure a payments system cannot absorb.
package ingestion

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// PaymentsAPI is the slice of Horizon this package needs.
type PaymentsAPI interface {
	Payments(horizonclient.OperationRequest) (operations.OperationsPage, error)
}

// Tuning defaults. Horizon rate-limits aggressively on the public instance, so
// the backoff ceiling is deliberately generous.
const (
	// DefaultPageSize is Horizon's maximum. Larger pages mean fewer round trips
	// and less time spent behind rate limits.
	DefaultPageSize uint = 200

	// DefaultPollInterval is how long to wait after catching up. Stellar closes
	// a ledger roughly every five seconds, so polling faster mostly buys
	// rate-limit pressure.
	DefaultPollInterval = 5 * time.Second

	minBackoff = 1 * time.Second
	maxBackoff = 2 * time.Minute
)

// Config describes an ingester.
type Config struct {
	// Stream names the cursor row, so several ingesters can track different
	// Horizon queries without colliding.
	Stream string

	// PageSize is the Horizon page size. Zero means DefaultPageSize.
	PageSize uint

	// PollInterval is the pause after catching up. Zero means DefaultPollInterval.
	PollInterval time.Duration
}

// Ingester reads payments from Horizon and posts them to the ledger.
type Ingester struct {
	horizon PaymentsAPI
	ledger  *ledger.Store
	pool    *pgxpool.Pool

	stream       string
	pageSize     uint
	pollInterval time.Duration

	// external is the ledger counterparty for value crossing the system
	// boundary. Resolved once at construction.
	external ledger.AccountID
}

// New returns an Ingester. It resolves the external account eagerly so that a
// misconfigured ledger fails at startup rather than on the first payment.
func New(
	ctx context.Context, h PaymentsAPI, l *ledger.Store, pool *pgxpool.Pool, cfg Config,
) (*Ingester, error) {
	if cfg.Stream == "" {
		return nil, errors.New("ingestion: stream name is required")
	}
	external, err := l.EnsureAccount(ctx, ledger.AccountExternal, "", "external")
	if err != nil {
		return nil, fmt.Errorf("ingestion: resolve external account: %w", err)
	}

	pageSize := cfg.PageSize
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	poll := cfg.PollInterval
	if poll == 0 {
		poll = DefaultPollInterval
	}

	return &Ingester{
		horizon: h, ledger: l, pool: pool,
		stream: cfg.Stream, pageSize: pageSize, pollInterval: poll,
		external: external,
	}, nil
}

// Track registers a Stellar address as belonging to a ledger account, so that
// payments to and from it are ingested.
func (i *Ingester) Track(ctx context.Context, address string, account ledger.AccountID) error {
	_, err := i.pool.Exec(ctx, `
		INSERT INTO stellar_accounts (address, ledger_account_id)
		VALUES ($1, $2)
		ON CONFLICT (address) DO NOTHING`,
		address, int64(account),
	)
	if err != nil {
		return fmt.Errorf("ingestion: track %s: %w", address, err)
	}
	return nil
}

// Run ingests continuously until ctx is cancelled.
//
// Errors are transient by assumption: Horizon goes away, the network blips, a
// rate limit bites. Backing off and retrying is correct because the cursor is
// durable, so nothing is lost by waiting. Returning on the first error would
// stop ingestion permanently on a blip.
func (i *Ingester) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		n, err := i.Once(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if sleepErr := sleep(ctx, jitter(backoff)); sleepErr != nil {
				return sleepErr
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		backoff = minBackoff
		// A short page means we have caught up with the chain; anything else
		// means there is more waiting and we should not pause.
		if uint(n) < i.pageSize {
			if err := sleep(ctx, i.pollInterval); err != nil {
				return err
			}
		}
	}
}

// Once processes at most one page and returns how many records it consumed,
// including records it deliberately skipped.
func (i *Ingester) Once(ctx context.Context) (int, error) {
	cursor, err := i.loadCursor(ctx)
	if err != nil {
		return 0, err
	}

	page, err := i.horizon.Payments(horizonclient.OperationRequest{
		Cursor: cursor,
		Order:  horizonclient.OrderAsc,
		Limit:  i.pageSize,
		// Failed transactions moved no money. Including them would post
		// entries for value that never changed hands.
		IncludeFailed: false,
	})
	if err != nil {
		return 0, fmt.Errorf("ingestion: fetch payments after %q: %w", cursor, err)
	}

	consumed := 0
	for _, record := range page.Embedded.Records {
		token := record.PagingToken()
		if err := i.ingestOne(ctx, record); err != nil {
			// Stop at the failure rather than skipping past it. The cursor is
			// still on the last good record, so the retry resumes here.
			return consumed, err
		}
		// Advance only after the ledger write has committed.
		if err := i.saveCursor(ctx, token); err != nil {
			return consumed, err
		}
		consumed++
	}
	return consumed, nil
}

// ingestOne posts a single operation. Anything that is not a payment we track
// is a no-op, but its cursor still advances — otherwise a single unrecognised
// record would wedge the stream forever.
func (i *Ingester) ingestOne(ctx context.Context, record operations.Operation) error {
	payment, ok := record.(operations.Payment)
	if !ok {
		return nil
	}
	if !payment.TransactionSuccessful {
		return nil
	}

	from, fromTracked, err := i.resolveAccount(ctx, payment.From)
	if err != nil {
		return err
	}
	to, toTracked, err := i.resolveAccount(ctx, payment.To)
	if err != nil {
		return err
	}
	if !fromTracked && !toTracked {
		return nil
	}

	amount, err := money.Parse(payment.Amount)
	if err != nil {
		return fmt.Errorf("ingestion: operation %s has unparseable amount %q: %w",
			payment.ID, payment.Amount, err)
	}
	if amount.Sign() <= 0 {
		return fmt.Errorf("ingestion: operation %s has non-positive amount %s", payment.ID, amount)
	}

	asset, err := i.resolveAsset(ctx, payment.Asset.Type, payment.Asset.Code, payment.Asset.Issuer)
	if err != nil {
		return err
	}

	kind, postings := i.postingsFor(fromTracked, toTracked, from, to, asset, amount)

	_, err = i.ledger.Post(ctx, ledger.PostRequest{
		// Derived from the Horizon operation id, so replaying a page cannot
		// post the same payment twice.
		IdempotencyKey: "horizon:op:" + payment.ID,
		Kind:           kind,
		ExternalRef:    payment.TransactionHash,
		// The chain's close time, not ours. It is the authoritative "when", and
		// it is stable across replays so the idempotency fingerprint matches.
		OccurredAt: payment.LedgerCloseTime,
		Postings:   postings,
	})
	if err != nil {
		return fmt.Errorf("ingestion: post operation %s: %w", payment.ID, err)
	}
	return nil
}

// postingsFor turns a payment into balanced ledger lines. Which accounts we
// track decides whether value entered, left, or moved within the system.
func (i *Ingester) postingsFor(
	fromTracked, toTracked bool,
	from, to ledger.AccountID,
	asset ledger.AssetID,
	amount money.Stroops,
) (ledger.TxKind, []ledger.Posting) {
	switch {
	case fromTracked && toTracked:
		// Internal transfer: no net change against the outside world.
		return ledger.TxSend, []ledger.Posting{
			{Account: from, Asset: asset, Amount: -amount},
			{Account: to, Asset: asset, Amount: amount},
		}
	case toTracked:
		return ledger.TxDeposit, []ledger.Posting{
			{Account: to, Asset: asset, Amount: amount},
			{Account: i.external, Asset: asset, Amount: -amount},
		}
	default:
		return ledger.TxWithdrawal, []ledger.Posting{
			{Account: from, Asset: asset, Amount: -amount},
			{Account: i.external, Asset: asset, Amount: amount},
		}
	}
}

func (i *Ingester) resolveAccount(ctx context.Context, address string) (ledger.AccountID, bool, error) {
	if address == "" {
		return 0, false, nil
	}
	var id int64
	err := i.pool.QueryRow(ctx,
		`SELECT ledger_account_id FROM stellar_accounts WHERE address = $1`, address,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ingestion: resolve account %s: %w", address, err)
	}
	return ledger.AccountID(id), true, nil
}

func (i *Ingester) resolveAsset(ctx context.Context, assetType, code, issuer string) (ledger.AssetID, error) {
	if assetType == "native" {
		return i.ledger.EnsureAsset(ctx, "XLM", "")
	}
	if code == "" || issuer == "" {
		return 0, fmt.Errorf("ingestion: issued asset is missing code or issuer (type %q)", assetType)
	}
	return i.ledger.EnsureAsset(ctx, code, issuer)
}

func (i *Ingester) loadCursor(ctx context.Context) (string, error) {
	var cursor string
	err := i.pool.QueryRow(ctx,
		`SELECT cursor FROM ingestion_cursors WHERE stream = $1`, i.stream,
	).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		// No cursor yet. An empty cursor asks Horizon to start from the
		// beginning of the stream.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ingestion: load cursor for %q: %w", i.stream, err)
	}
	return cursor, nil
}

func (i *Ingester) saveCursor(ctx context.Context, cursor string) error {
	_, err := i.pool.Exec(ctx, `
		INSERT INTO ingestion_cursors (stream, cursor, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (stream) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = now()`,
		i.stream, cursor,
	)
	if err != nil {
		return fmt.Errorf("ingestion: save cursor for %q: %w", i.stream, err)
	}
	return nil
}

// jitter spreads retries so that several ingesters recovering from the same
// Horizon outage do not resynchronise into a thundering herd.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
