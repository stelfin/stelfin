package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

// ErrNoReclaim reports a reclaim that is unknown, expired or already submitted.
//
// One error for all three, the same reasoning as ErrNoChallenge: which it was
// is not information the presenter needs, and telling them narrows a guess.
var ErrNoReclaim = errors.New("store: no live reclaim")

// Reclaim is a transaction that hands a provisioned account back.
type Reclaim struct {
	Hash        string
	Org         ledger.OrgID
	OwnerRef    string
	Address     string
	Destination string
	XDR         string
	ExpiresAt   time.Time
}

// SaveReclaim records a built reclaim, superseding any live one for the same
// account.
//
// Superseding rather than accumulating: two envelopes that each sweep the same
// balance to the same place can only ever have one of them land, and the rest
// fail later for a reason nobody would connect to this.
func (s *Store) SaveReclaim(ctx context.Context, r Reclaim) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM pending_reclaims
		 WHERE org_id = $1 AND address = $2 AND submitted_at IS NULL`,
		int64(r.Org), r.Address); err != nil {
		return fmt.Errorf("store: supersede reclaim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_reclaims (
			hash, org_id, owner_ref, address, destination, envelope_xdr, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.Hash, int64(r.Org), r.OwnerRef, r.Address, r.Destination, r.XDR, r.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store: save reclaim: %w", err)
	}
	return tx.Commit(ctx)
}

// LiveReclaim returns an unsubmitted, unexpired reclaim.
func (s *Store) LiveReclaim(ctx context.Context, org ledger.OrgID, hash string) (Reclaim, error) {
	var r Reclaim
	err := s.pool.QueryRow(ctx, `
		SELECT hash, org_id, owner_ref, address, destination, envelope_xdr, expires_at
		  FROM pending_reclaims
		 WHERE hash = $1 AND org_id = $2 AND submitted_at IS NULL AND expires_at > now()`,
		hash, int64(org),
	).Scan(&r.Hash, &r.Org, &r.OwnerRef, &r.Address, &r.Destination, &r.XDR, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reclaim{}, fmt.Errorf("%w: %s", ErrNoReclaim, hash)
	}
	if err != nil {
		return Reclaim{}, fmt.Errorf("store: live reclaim %s: %w", hash, err)
	}
	return r, nil
}

// ClaimReclaim marks a reclaim as submitted, exactly once.
//
// A compare-and-set on submitted_at, so two submissions of the same signed
// envelope produce one attempt and one plain refusal. The account is about to
// be deleted; a second submission cannot double-spend, but it can produce a
// second confusing failure and a second attempt to release the same grant.
func (s *Store) ClaimReclaim(ctx context.Context, org ledger.OrgID, hash string) (Reclaim, error) {
	var r Reclaim
	err := s.pool.QueryRow(ctx, `
		UPDATE pending_reclaims SET submitted_at = now()
		 WHERE hash = $1 AND org_id = $2 AND submitted_at IS NULL AND expires_at > now()
		RETURNING hash, org_id, owner_ref, address, destination, envelope_xdr, expires_at`,
		hash, int64(org),
	).Scan(&r.Hash, &r.Org, &r.OwnerRef, &r.Address, &r.Destination, &r.XDR, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reclaim{}, fmt.Errorf("%w: %s", ErrNoReclaim, hash)
	}
	if err != nil {
		return Reclaim{}, fmt.Errorf("store: claim reclaim %s: %w", hash, err)
	}
	return r, nil
}

// ReleaseAddress forgets an account this org tracked, after it has been merged
// away.
//
// The ledger account stays. Its history is what explains every balance that
// ever passed through, and deleting it would leave postings referring to
// nothing — the address mapping is what stops ingestion from attributing a
// future payment to an account that no longer exists.
func (s *Store) ReleaseAddress(ctx context.Context, org ledger.OrgID, address string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM tracked_addresses WHERE org_id = $1 AND address = $2`,
		int64(org), address); err != nil {
		return fmt.Errorf("store: release address: %w", err)
	}
	// The member keeps their identity and their role, and loses the address
	// they no longer control. Removing the member instead would erase who
	// approved what.
	if _, err := tx.Exec(ctx, `
		UPDATE members
		   SET address = NULL, address_source = NULL, address_verified_at = NULL
		 WHERE org_id = $1 AND address = $2`,
		int64(org), address); err != nil {
		return fmt.Errorf("store: clear member address: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE enrollment_grants SET reclaimed_at = now()
		 WHERE org_id = $1 AND address = $2 AND reclaimed_at IS NULL`,
		int64(org), address); err != nil {
		return fmt.Errorf("store: mark grant reclaimed: %w", err)
	}
	return tx.Commit(ctx)
}
