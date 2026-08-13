// Package ledger is stelfin's append-only, double-entry record of value
// movement.
//
// It is an *index* of on-chain state, not the authority for it: Stellar decides
// what a balance is, and this ledger's job is to explain how it got there and to
// surface disagreement during reconciliation. Nothing here may be treated as
// proof that money exists.
//
// Every structural invariant is enforced by the database (see
// migrations/00001_ledger_core.sql). The checks in this package are a fast,
// well-worded first line of defence, never the only one — a future service, a
// psql session or a migration could bypass Go, but not the constraints.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stelfin/stelfin/internal/money"
)

// SQLSTATE codes raised by the ledger's triggers. Custom codes let us classify
// failures precisely instead of matching on message text.
const (
	sqlStateUnbalanced      = "ST001"
	sqlStateNegativeBalance = "ST002"
)

// AccountKind identifies what a ledger account represents. These are internal
// bookkeeping accounts and are never Stellar accounts.
type AccountKind string

const (
	// AccountUser is a stelfin user's position.
	AccountUser AccountKind = "user"
	// AccountTreasury is the XLM float backing sponsorship and fee-bumps.
	AccountTreasury AccountKind = "treasury"
	// AccountExternal is the outside world: the counterparty for value crossing
	// the system boundary. It is the only account permitted to go negative.
	AccountExternal AccountKind = "external"
	// AccountFeeExpense accumulates fees paid.
	AccountFeeExpense AccountKind = "fee_expense"
	// AccountSponsoredReserve tracks CAP-33 reserves locked against user
	// accounts: reclaimable, but not spendable float.
	AccountSponsoredReserve AccountKind = "sponsored_reserve"
)

// TxKind classifies a journal entry.
type TxKind string

const (
	TxDeposit        TxKind = "deposit"
	TxSend           TxKind = "send"
	TxFee            TxKind = "fee"
	TxSponsor        TxKind = "sponsor"
	TxReserveRelease TxKind = "reserve_release"
	TxWithdrawal     TxKind = "withdrawal"
)

type (
	// AccountID identifies a ledger account.
	AccountID int64
	// AssetID identifies a registered asset.
	AssetID int16
	// TxID identifies a posted transaction.
	TxID int64
)

var (
	// ErrNoPostings reports a transaction with no lines.
	ErrNoPostings = errors.New("ledger: transaction has no postings")

	// ErrZeroAmount reports a posting of zero, which carries no information and
	// usually means a bug upstream.
	ErrZeroAmount = errors.New("ledger: posting has zero amount")

	// ErrUnbalanced reports postings that do not sum to zero for some asset.
	ErrUnbalanced = errors.New("ledger: postings do not sum to zero per asset")

	// ErrInsufficientFunds reports an account that would be driven negative.
	// Only the external account may hold a negative balance.
	ErrInsufficientFunds = errors.New("ledger: account would go negative")

	// ErrIdempotencyKeyReused reports a replay carrying different content than
	// the original. This is never a safe retry: it means a key was reused for
	// different money.
	ErrIdempotencyKeyReused = errors.New("ledger: idempotency key reused with different content")
)

// Posting is one line of a journal entry. Amount is signed: a balanced
// transaction sums to zero for each asset it touches.
type Posting struct {
	Account AccountID
	Asset   AssetID
	Amount  money.Stroops
}

// PostRequest describes a transaction to record.
type PostRequest struct {
	// IdempotencyKey makes posting safe to retry. Replaying the same key with
	// the same content returns the original transaction; replaying it with
	// different content is an error.
	IdempotencyKey string

	Kind TxKind

	// ExternalRef is the Stellar transaction hash, lowercase hex, or empty when
	// not yet known.
	ExternalRef string

	// OccurredAt is when the event happened — for on-chain movement, the ledger
	// close time. It must be derived from the event, never from the clock at
	// call time: it contributes to the idempotency fingerprint, so a wall-clock
	// value would make an otherwise identical retry look like different content.
	OccurredAt time.Time

	// Metadata is arbitrary JSON. Nil is stored as an empty object. It must
	// never carry PII: this table is retained for audit indefinitely.
	Metadata []byte

	Postings []Posting
}

// Store is a handle on the ledger's database.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool. The caller retains ownership of pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Post records a balanced transaction atomically and returns its id.
//
// The zero-sum invariant is checked here for a clear error message and again by
// a deferred database constraint at COMMIT, which is the check that actually
// guarantees it.
func (s *Store) Post(ctx context.Context, req PostRequest) (TxID, error) {
	if err := validate(req); err != nil {
		return 0, err
	}
	fingerprint := req.fingerprint()

	metadata := req.Metadata
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}
	var externalRef *string
	if req.ExternalRef != "" {
		externalRef = &req.ExternalRef
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("ledger: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id TxID
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions
			(idempotency_key, request_fingerprint, kind, external_ref, occurred_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		req.IdempotencyKey, fingerprint[:], string(req.Kind), externalRef,
		req.OccurredAt, metadata,
	).Scan(&id)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key already exists. This is the retry path: confirm the content
		// matches and hand back the original rather than posting again.
		//
		// This must run on tx, not on the pool. ON CONFLICT DO NOTHING does not
		// abort the transaction, so tx is still usable — and reaching for a
		// second pooled connection here while this one is still held would
		// deadlock the pool as soon as concurrent callers replay the same key.
		return resolveReplay(ctx, tx, req.IdempotencyKey, fingerprint)
	case err != nil:
		return 0, fmt.Errorf("ledger: insert transaction: %w", classify(err))
	}

	for i, p := range req.Postings {
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, asset_id, amount)
			VALUES ($1, $2, $3, $4)`,
			id, int64(p.Account), int16(p.Asset), int64(p.Amount),
		)
		if err != nil {
			return 0, fmt.Errorf("ledger: insert posting %d of %d: %w",
				i+1, len(req.Postings), classify(err))
		}
	}

	// The zero-sum constraint is DEFERRABLE INITIALLY DEFERRED, so it fires
	// here rather than on any individual insert. Commit errors are therefore
	// load-bearing and must not be swallowed.
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("ledger: commit: %w", classify(err))
	}
	return id, nil
}

// resolveReplay handles a collision on the idempotency key. It runs on the
// caller's transaction so that no second pooled connection is needed: acquiring
// one while the caller still holds theirs is a pool deadlock under concurrency.
func resolveReplay(ctx context.Context, tx pgx.Tx, key string, want [32]byte) (TxID, error) {
	var id TxID
	var got []byte
	err := tx.QueryRow(ctx, `
		SELECT id, request_fingerprint FROM ledger_transactions WHERE idempotency_key = $1`,
		key,
	).Scan(&id, &got)
	if err != nil {
		return 0, fmt.Errorf("ledger: resolve replay of key %q: %w", key, err)
	}

	if len(got) != len(want) || string(got) != string(want[:]) {
		return 0, fmt.Errorf("%w: key %q already posted as transaction %d with different content",
			ErrIdempotencyKeyReused, key, id)
	}
	return id, nil
}

// Balance returns the current balance of an account in one asset. An account
// with no entries for that asset reads as zero.
func (s *Store) Balance(ctx context.Context, account AccountID, asset AssetID) (money.Stroops, error) {
	var balance int64
	err := s.pool.QueryRow(ctx, `
		SELECT balance FROM ledger_balances WHERE account_id = $1 AND asset_id = $2`,
		int64(account), int16(asset),
	).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: balance of account %d asset %d: %w", account, asset, err)
	}
	return money.Stroops(balance), nil
}

// EnsureAsset registers an asset if it is not already known and returns its id.
// Pass an empty issuer for the native asset.
func (s *Store) EnsureAsset(ctx context.Context, code, issuer string) (AssetID, error) {
	native := issuer == ""
	var issuerArg *string
	if !native {
		issuerArg = &issuer
	}

	var id AssetID
	err := s.pool.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO assets (code, issuer, is_native)
			VALUES ($1, $2, $3)
			ON CONFLICT (code, COALESCE(issuer, '')) DO NOTHING
			RETURNING id
		)
		SELECT id FROM inserted
		UNION ALL
		SELECT id FROM assets WHERE code = $1 AND COALESCE(issuer, '') = COALESCE($2, '')
		LIMIT 1`,
		code, issuerArg, native,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ledger: ensure asset %s: %w", code, err)
	}
	return id, nil
}

// EnsureAccount registers a ledger account if absent and returns its id.
// ownerRef must be set for AccountUser and empty for every other kind.
func (s *Store) EnsureAccount(ctx context.Context, kind AccountKind, ownerRef, name string) (AccountID, error) {
	var ownerArg *string
	if ownerRef != "" {
		ownerArg = &ownerRef
	}
	allowsNegative := kind == AccountExternal

	// Singleton kinds conflict on kind; user accounts conflict on owner_ref.
	// Both partial indexes are covered by re-selecting on the same predicate.
	var id AccountID
	err := s.pool.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO ledger_accounts (kind, owner_ref, name, allows_negative)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING
			RETURNING id
		)
		SELECT id FROM inserted
		UNION ALL
		SELECT id FROM ledger_accounts
		 WHERE kind = $1 AND COALESCE(owner_ref, '') = COALESCE($2, '')
		LIMIT 1`,
		string(kind), ownerArg, name, allowsNegative,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ledger: ensure account %s/%s: %w", kind, ownerRef, err)
	}
	return id, nil
}

func validate(req PostRequest) error {
	if req.IdempotencyKey == "" {
		return errors.New("ledger: idempotency key is required")
	}
	if req.Kind == "" {
		return errors.New("ledger: transaction kind is required")
	}
	if req.OccurredAt.IsZero() {
		return errors.New("ledger: occurred_at is required and must come from the event")
	}
	if len(req.Postings) == 0 {
		return ErrNoPostings
	}

	perAsset := make(map[AssetID][]money.Stroops)
	for i, p := range req.Postings {
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: posting %d of %d", ErrZeroAmount, i+1, len(req.Postings))
		}
		perAsset[p.Asset] = append(perAsset[p.Asset], p.Amount)
	}

	for asset, amounts := range perAsset {
		total, err := money.Sum(amounts...)
		if err != nil {
			return fmt.Errorf("ledger: asset %d: %w", asset, err)
		}
		if !total.IsZero() {
			return fmt.Errorf("%w: asset %d sums to %s", ErrUnbalanced, asset, total)
		}
	}
	return nil
}

// fingerprint is a canonical SHA-256 over the semantic content of the request.
// Postings are sorted first, so two requests that differ only in the order of
// their lines are recognised as the same transaction.
func (r PostRequest) fingerprint() [32]byte {
	postings := make([]Posting, len(r.Postings))
	copy(postings, r.Postings)
	sort.Slice(postings, func(i, j int) bool {
		a, b := postings[i], postings[j]
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Asset != b.Asset {
			return a.Asset < b.Asset
		}
		return a.Amount < b.Amount
	})

	h := sha256.New()
	// Length-prefix every variable-width field so that concatenation is
	// unambiguous: without it, ("ab","c") and ("a","bc") would hash alike.
	writeString := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	writeInt := func(v int64) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(v))
		h.Write(n[:])
	}

	writeString(string(r.Kind))
	writeString(r.ExternalRef)
	writeInt(r.OccurredAt.UTC().UnixNano())
	writeInt(int64(len(postings)))
	for _, p := range postings {
		writeInt(int64(p.Account))
		writeInt(int64(p.Asset))
		writeInt(int64(p.Amount))
	}
	return [32]byte(h.Sum(nil))
}

// classify maps the ledger's custom SQLSTATEs onto package-level sentinel
// errors so callers can branch without inspecting driver types.
func classify(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case sqlStateUnbalanced:
		return fmt.Errorf("%w (%s)", ErrUnbalanced, pgErr.Message)
	case sqlStateNegativeBalance:
		return fmt.Errorf("%w (%s)", ErrInsufficientFunds, pgErr.Message)
	default:
		return err
	}
}
