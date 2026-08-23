package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// ErrAddressNotTracked reports an address this deployment does not index.
var ErrAddressNotTracked = errors.New("store: address is not tracked")

// Address roles.
const (
	// RoleTreasury is an org's own account.
	RoleTreasury = "treasury"
	// RoleMember is a member's personal account.
	RoleMember = "member"
	// RoleSponsor is the operator's fee and reserve account.
	RoleSponsor = "sponsor"
)

// querier is satisfied by both a pool and a transaction, so a query can be
// written once and run inside a caller's transaction when atomicity matters.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ensureAccountTx is EnsureAccount against a caller's transaction.
//
// Duplicated from the ledger engine rather than exported from it, because the
// engine's version takes the pool: calling it while holding a transaction would
// reach for a second pooled connection and deadlock under concurrency, which is
// a bug this codebase has already had once.
func ensureAccountTx(
	ctx context.Context, q querier, org ledger.OrgID, kind ledger.AccountKind, ownerRef, name string,
) (ledger.AccountID, error) {
	var ownerArg *string
	if ownerRef != "" {
		ownerArg = &ownerRef
	}
	allowsNegative := kind == ledger.AccountExternal || kind == ledger.AccountTrading

	var id ledger.AccountID
	err := q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO ledger_accounts (org_id, kind, owner_ref, name, allows_negative)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT DO NOTHING
			RETURNING id
		)
		SELECT id FROM inserted
		UNION ALL
		SELECT id FROM ledger_accounts
		 WHERE org_id = $1
		   AND kind = $2
		   AND COALESCE(owner_ref, '') = COALESCE($3, '')
		   AND (kind <> 'treasury' OR lower(name) = lower($4))
		LIMIT 1`,
		int64(org), string(kind), ownerArg, name, allowsNegative,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: ensure account %s in org %d: %w", kind, org, err)
	}
	return id, nil
}

// EnsureAccount registers a ledger account if absent.
func (s *Store) EnsureAccount(
	ctx context.Context, org ledger.OrgID, kind ledger.AccountKind, ownerRef, name string,
) (ledger.AccountID, error) {
	return ensureAccountTx(ctx, s.pool, org, kind, ownerRef, name)
}

// AccountFor returns the ledger account holding an owner's position, if it has
// one.
func (s *Store) AccountFor(ctx context.Context, org ledger.OrgID, ownerRef string) (ledger.AccountID, bool, error) {
	var id ledger.AccountID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM ledger_accounts
		 WHERE org_id = $1 AND kind = 'member' AND owner_ref = $2`,
		int64(org), ownerRef,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: account for %s: %w", ownerRef, err)
	}
	return id, true, nil
}

// TrackedAddress is an address this deployment indexes.
type TrackedAddress struct {
	Address string
	Org     ledger.OrgID
	Account ledger.AccountID
	Role    string
}

// TrackAddress registers an address against a ledger account.
//
// One address per account and one account per address: a payment arriving has
// to post to exactly one place, and any other mapping makes ingestion ambiguous
// in a way that shows up as a wrong balance rather than an error.
func (s *Store) TrackAddress(
	ctx context.Context, org ledger.OrgID, address string, account ledger.AccountID, role string,
) error {
	return trackAddressTx(ctx, s.pool, org, address, account, role)
}

func trackAddressTx(
	ctx context.Context, q querier, org ledger.OrgID, address string, account ledger.AccountID, role string,
) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tracked_addresses (address, org_id, ledger_account_id, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (address) DO NOTHING`,
		address, int64(org), int64(account), role)
	if err != nil {
		return fmt.Errorf("store: track address: %w", err)
	}
	return nil
}

// TrackedAddress resolves an on-chain address to the org and account it
// belongs to.
//
// Deliberately not org-scoped: ingestion sees an address before it knows whose
// it is, and this lookup is what tells it. Every caller uses the org it returns
// rather than one it already had.
func (s *Store) TrackedAddress(ctx context.Context, address string) (TrackedAddress, bool, error) {
	var a TrackedAddress
	err := s.pool.QueryRow(ctx, `
		SELECT address, org_id, ledger_account_id, role
		  FROM tracked_addresses WHERE address = $1`, address,
	).Scan(&a.Address, &a.Org, &a.Account, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return TrackedAddress{}, false, nil
	}
	if err != nil {
		return TrackedAddress{}, false, fmt.Errorf("store: tracked address %s: %w", address, err)
	}
	return a, true, nil
}

// AddressForOwner returns the Stellar address an owner transacts from.
func (s *Store) AddressForOwner(ctx context.Context, org ledger.OrgID, ownerRef string) (string, error) {
	var address string
	err := s.pool.QueryRow(ctx, `
		SELECT t.address
		  FROM tracked_addresses t
		  JOIN ledger_accounts a ON a.id = t.ledger_account_id
		 WHERE a.org_id = $1 AND a.kind = 'member' AND a.owner_ref = $2`,
		int64(org), ownerRef,
	).Scan(&address)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrAddressNotTracked, ownerRef)
	}
	if err != nil {
		return "", fmt.Errorf("store: address for %s: %w", ownerRef, err)
	}
	return address, nil
}

// HasAddress reports whether an owner has a tracked Stellar address.
func (s *Store) HasAddress(ctx context.Context, org ledger.OrgID, ownerRef string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM tracked_addresses t
			  JOIN ledger_accounts a ON a.id = t.ledger_account_id
			 WHERE a.org_id = $1 AND a.kind = 'member' AND a.owner_ref = $2)`,
		int64(org), ownerRef,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: has address %s: %w", ownerRef, err)
	}
	return exists, nil
}

// AssetBalance is one asset's balance on one account.
type AssetBalance struct {
	Asset   ledger.AssetID
	Code    string
	Issuer  string
	Balance money.Stroops
}

// Post records a balanced transaction. The org on the request is authoritative.
func (s *Store) Post(ctx context.Context, req ledger.PostRequest) (ledger.TxID, error) {
	return s.ledger.Post(ctx, req)
}

// Balance returns one account's balance in one asset.
func (s *Store) Balance(
	ctx context.Context, org ledger.OrgID, account ledger.AccountID, asset ledger.AssetID,
) (money.Stroops, error) {
	return s.ledger.Balance(ctx, org, account, asset)
}

// Balances returns every non-zero balance on one account.
func (s *Store) Balances(ctx context.Context, org ledger.OrgID, account ledger.AccountID) ([]AssetBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.asset_id, s.code, COALESCE(s.issuer, ''), b.balance
		  FROM ledger_balances b
		  JOIN ledger_accounts a ON a.id = b.account_id
		  JOIN assets s ON s.id = b.asset_id
		 WHERE b.account_id = $1 AND a.org_id = $2 AND b.balance <> 0
		 ORDER BY s.code`,
		int64(account), int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: balances: %w", err)
	}
	defer rows.Close()

	var out []AssetBalance
	for rows.Next() {
		var b AssetBalance
		if err := rows.Scan(&b.Asset, &b.Code, &b.Issuer, &b.Balance); err != nil {
			return nil, fmt.Errorf("store: scan balance: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TreasuryLine is one treasury account's holding of one asset.
type TreasuryLine struct {
	Account ledger.AccountID
	Name    string
	Asset   ledger.AssetID
	Code    string
	Balance money.Stroops
}

// TreasurySnapshot reports what an org holds, across every treasury account it
// runs. A DAO with an ops wallet and a grants wallet sees both.
func (s *Store) TreasurySnapshot(ctx context.Context, org ledger.OrgID) ([]TreasuryLine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, b.asset_id, s.code, b.balance
		  FROM ledger_accounts a
		  JOIN ledger_balances b ON b.account_id = a.id
		  JOIN assets s ON s.id = b.asset_id
		 WHERE a.org_id = $1 AND a.kind = 'treasury' AND b.balance <> 0
		 ORDER BY a.name, s.code`,
		int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: treasury snapshot: %w", err)
	}
	defer rows.Close()

	var out []TreasuryLine
	for rows.Next() {
		var l TreasuryLine
		if err := rows.Scan(&l.Account, &l.Name, &l.Asset, &l.Code, &l.Balance); err != nil {
			return nil, fmt.Errorf("store: scan treasury line: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
