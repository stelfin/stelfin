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
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

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
	// PageSize is how many records to ask a source for at a time. Zero means
	// DefaultPageSize.
	PageSize uint

	// PollInterval is the pause after catching up. Zero means DefaultPollInterval.
	PollInterval time.Duration
}

// Ingester applies records from any source to the ledger.
//
// It holds no source of its own. Which chains and which endpoints a deployment
// watches is a wiring decision, and one ingester serving several sources keeps
// the idempotency and the cursor discipline in a single place instead of once
// per stream.
type Ingester struct {
	store *store.Store
	pool  *pgxpool.Pool

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
func New(s *store.Store, pool *pgxpool.Pool, cfg Config) (*Ingester, error) {
	if s == nil || pool == nil {
		return nil, errors.New("ingestion: a store and a pool are required")
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
		store: s, pool: pool,
		pageSize: pageSize, pollInterval: poll,
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
func (i *Ingester) Run(ctx context.Context, src Source) error {
	backoff := minBackoff
	for {
		n, err := i.Once(ctx, src)
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
func (i *Ingester) Once(ctx context.Context, src Source) (int, error) {
	stream := src.Name()
	cursor, err := i.loadCursor(ctx, stream)
	if err != nil {
		return 0, err
	}

	records, err := src.Fetch(ctx, cursor, i.pageSize)
	if err != nil {
		return 0, err
	}

	consumed := 0
	for _, record := range records {
		if err := i.apply(ctx, record); err != nil {
			// Stop at the failure rather than skipping past it. The cursor is
			// still on the last good record, so the retry resumes here.
			return consumed, err
		}
		// Advance only after the ledger write has committed. The reverse order
		// is at-most-once and silently loses payments, which is the one failure
		// a payments system cannot absorb.
		if err := i.saveCursor(ctx, stream, record.Cursor); err != nil {
			return consumed, err
		}
		consumed++
	}
	return consumed, nil
}

// apply posts one record.
//
// A record naming nothing this deployment tracks is a no-op whose cursor still
// advances — otherwise one unrecognised record wedges the stream forever, and
// the stream carries every other tenant's money too.
func (i *Ingester) apply(ctx context.Context, record Record) error {
	for n, movement := range record.Movements {
		if err := i.applyMovement(ctx, record, n, movement); err != nil {
			return err
		}
	}
	if record.Closed != "" {
		if err := i.applyClose(ctx, record.Closed); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) applyMovement(
	ctx context.Context, record Record, n int, movement Movement,
) error {
	from, fromTracked, err := i.store.TrackedAddress(ctx, movement.From)
	if err != nil {
		return err
	}
	to, toTracked, err := i.store.TrackedAddress(ctx, movement.To)
	if err != nil {
		return err
	}
	if !fromTracked && !toTracked {
		return nil
	}

	if movement.Amount.Sign() <= 0 {
		return fmt.Errorf("ingestion: record %s has non-positive amount %s",
			record.ID, movement.Amount)
	}

	asset, err := i.resolveAsset(ctx,
		movement.AssetType, movement.AssetCode, movement.AssetIssuer)
	if err != nil {
		return err
	}

	entries, err := i.entriesFor(ctx, fromTracked, toTracked, from, to, asset, movement.Amount)
	if err != nil {
		return err
	}

	// The movement index is part of the key, because one record can carry more
	// than one movement and they must not collide. Without it a second leg
	// would look like a duplicate of the first and be dropped.
	key := record.ID
	if len(record.Movements) > 1 {
		key = fmt.Sprintf("%s:%d", record.ID, n)
	}

	for _, e := range entries {
		_, err = i.store.Post(ctx, ledger.PostRequest{
			Org: e.org,
			// Derived from the chain's own identifier, so replaying a page
			// cannot post the same movement twice. Unique per org, which is
			// what lets one operation be recorded in both tenants' books when
			// value crosses between them.
			IdempotencyKey: key,
			Kind:           e.kind,
			ExternalRef:    record.TxHash,
			OccurredAt:     record.OccurredAt,
			Postings:       e.postings,
		})
		if err != nil {
			return fmt.Errorf("ingestion: post record %s for org %d: %w", record.ID, e.org, err)
		}
	}
	return nil
}

// applyClose forgets an address that no longer exists on chain.
//
// A member who merges their provisioned account themselves, outside stelfin,
// leaves this deployment believing in an account the network has deleted — and
// still holding a reserve grant against it. The chain is the authority on
// whether an account exists, so this follows it.
func (i *Ingester) applyClose(ctx context.Context, address string) error {
	tracked, ok, err := i.store.TrackedAddress(ctx, address)
	if err != nil || !ok {
		return err
	}
	return i.store.ReleaseAddress(ctx, tracked.Org, address)
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

func (i *Ingester) loadCursor(ctx context.Context, stream string) (string, error) {
	var cursor string
	err := i.pool.QueryRow(ctx,
		`SELECT cursor FROM ingestion_cursors WHERE stream = $1`, stream,
	).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		// No cursor yet. An empty cursor asks Horizon to start from the
		// beginning of the stream.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ingestion: load cursor for %q: %w", stream, err)
	}
	return cursor, nil
}

func (i *Ingester) saveCursor(ctx context.Context, stream, cursor string) error {
	_, err := i.pool.Exec(ctx, `
		INSERT INTO ingestion_cursors (stream, cursor, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (stream) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = now()`,
		stream, cursor,
	)
	if err != nil {
		return fmt.Errorf("ingestion: save cursor for %q: %w", stream, err)
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
