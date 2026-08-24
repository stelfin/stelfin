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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
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
	store   *store.Store
	pool    *pgxpool.Pool

	stream       string
	pageSize     uint
	pollInterval time.Duration

	// external is the counterparty account for each org, memoised.
	//
	// Per org, not global: value crossing one tenant's boundary is nothing to
	// do with another's, and a shared counterparty would make every org's
	// external balance the sum of everybody's.
	mu       sync.Mutex
	external map[ledger.OrgID]ledger.AccountID
}

// New returns an Ingester.
func New(
	ctx context.Context, h PaymentsAPI, s *store.Store, pool *pgxpool.Pool, cfg Config,
) (*Ingester, error) {
	if cfg.Stream == "" {
		return nil, errors.New("ingestion: stream name is required")
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
		horizon: h, store: s, pool: pool,
		stream: cfg.Stream, pageSize: pageSize, pollInterval: poll,
		external: make(map[ledger.OrgID]ledger.AccountID),
	}, nil
}

// Track registers a Stellar address as belonging to a ledger account, so that
// payments to and from it are ingested.
func (i *Ingester) Track(
	ctx context.Context, org ledger.OrgID, address string, account ledger.AccountID, role string,
) error {
	return i.store.TrackAddress(ctx, org, address, account, role)
}

// externalFor returns an org's counterparty account, memoised.
func (i *Ingester) externalFor(ctx context.Context, org ledger.OrgID) (ledger.AccountID, error) {
	i.mu.Lock()
	if id, ok := i.external[org]; ok {
		i.mu.Unlock()
		return id, nil
	}
	i.mu.Unlock()

	id, err := i.store.EnsureAccount(ctx, org, ledger.AccountExternal, "", "external")
	if err != nil {
		return 0, fmt.Errorf("ingestion: external account for org %d: %w", org, err)
	}

	i.mu.Lock()
	i.external[org] = id
	i.mu.Unlock()
	return id, nil
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

	from, fromTracked, err := i.store.TrackedAddress(ctx, payment.From)
	if err != nil {
		return err
	}
	to, toTracked, err := i.store.TrackedAddress(ctx, payment.To)
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

	entries, err := i.entriesFor(ctx, fromTracked, toTracked, from, to, asset, amount)
	if err != nil {
		return err
	}

	for _, e := range entries {
		_, err = i.store.Post(ctx, ledger.PostRequest{
			Org: e.org,
			// Derived from the Horizon operation id, so replaying a page cannot
			// post the same payment twice. Unique per org, which is what lets
			// one operation be recorded in both tenants' books when a payment
			// crosses between them.
			IdempotencyKey: "horizon:op:" + payment.ID,
			Kind:           e.kind,
			ExternalRef:    payment.TransactionHash,
			// The chain's close time, not ours. It is the authoritative "when",
			// and it is stable across replays so the fingerprint matches.
			OccurredAt: payment.LedgerCloseTime,
			Postings:   e.postings,
		})
		if err != nil {
			return fmt.Errorf("ingestion: post operation %s for org %d: %w", payment.ID, e.org, err)
		}
	}
	return nil
}

// entry is one org's record of an operation.
type entry struct {
	org      ledger.OrgID
	kind     ledger.TxKind
	postings []ledger.Posting
}

// entriesFor turns a payment into the balanced ledger lines each affected org
// records.
//
// Usually one org, sometimes two. A payment between two tenants' tracked
// addresses is not one transaction spanning both — the database refuses that,
// and rightly, because one DAO's books would then contain another's account.
// It is a withdrawal from one org and a deposit into the other, each balanced
// against that org's own counterparty account. That is what "value crossing
// between tenants leaves one org and enters the other" means in practice.
func (i *Ingester) entriesFor(
	ctx context.Context,
	fromTracked, toTracked bool,
	from, to store.TrackedAddress,
	asset ledger.AssetID,
	amount money.Stroops,
) ([]entry, error) {
	if fromTracked && toTracked && from.Org == to.Org {
		// Internal transfer within one org: no net change against the outside.
		return []entry{{
			org:  from.Org,
			kind: ledger.TxSend,
			postings: []ledger.Posting{
				{Account: from.Account, Asset: asset, Amount: -amount},
				{Account: to.Account, Asset: asset, Amount: amount},
			},
		}}, nil
	}

	var out []entry
	if fromTracked {
		external, err := i.externalFor(ctx, from.Org)
		if err != nil {
			return nil, err
		}
		out = append(out, entry{
			org:  from.Org,
			kind: ledger.TxWithdrawal,
			postings: []ledger.Posting{
				{Account: from.Account, Asset: asset, Amount: -amount},
				{Account: external, Asset: asset, Amount: amount},
			},
		})
	}
	if toTracked {
		external, err := i.externalFor(ctx, to.Org)
		if err != nil {
			return nil, err
		}
		out = append(out, entry{
			org:  to.Org,
			kind: ledger.TxDeposit,
			postings: []ledger.Posting{
				{Account: to.Account, Asset: asset, Amount: amount},
				{Account: external, Asset: asset, Amount: -amount},
			},
		})
	}
	return out, nil
}

func (i *Ingester) resolveAsset(ctx context.Context, assetType, code, issuer string) (ledger.AssetID, error) {
	if assetType == "native" {
		return i.store.Ledger().EnsureAsset(ctx, "XLM", "")
	}
	if code == "" || issuer == "" {
		return 0, fmt.Errorf("ingestion: issued asset is missing code or issuer (type %q)", assetType)
	}
	return i.store.Ledger().EnsureAsset(ctx, code, issuer)
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
