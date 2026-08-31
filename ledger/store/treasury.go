package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

// Treasury kinds.
const (
	// TreasuryClassic is a G-account whose authority is signer weight.
	TreasuryClassic = "classic"
	// TreasuryContract is a C-address whose authority is contract code.
	//
	// The distinction is not cosmetic. A policy contract can enforce spending
	// rules on a contract account and can enforce nothing at all on a classic
	// M-of-N account, where any M signers bypass it. The product has to be able
	// to say which one a DAO is running, so the two are different values rather
	// than a nullable contract column.
	TreasuryContract = "contract"
)

// ErrNoTreasury reports a treasury this org has not linked.
var ErrNoTreasury = errors.New("store: no such treasury")

// TreasuryID identifies a linked treasury.
type TreasuryID int64

// Treasury is an account an org has proved control of.
type Treasury struct {
	ID      TreasuryID
	Org     ledger.OrgID
	Kind    string
	Address string
	Label   string

	// Thresholds as the account reported them when it was last read.
	//
	// Advisory, and stale by construction. Nothing may decide that an envelope
	// is sufficiently signed from these — the network decides, against the
	// account as it stands at inclusion, and a signer removed a minute ago is
	// exactly the case someone would exploit. They are here so the bot can say
	// "2 of 3" without a round trip per keystroke.
	Low, Medium, High int32

	Contract    string
	VerifiedAt  time.Time
	RefreshedAt time.Time
}

// LinkTreasuryParams records a treasury whose control has been proved.
type LinkTreasuryParams struct {
	Org      ledger.OrgID
	Kind     string
	Address  string
	Label    string
	Contract string

	Low, Medium, High int32

	// VerifiedBy is the identity that presented the signed challenge. Nullable
	// in the schema because an identity may later be deleted; the proof stays.
	VerifiedBy IdentityID
}

// LinkTreasury records a treasury, or refreshes one already linked.
//
// Only ever called after a SEP-10 challenge has been verified to the account's
// medium threshold — the same weight needed to move its money. A row here is a
// proof rather than a claim, which is the whole reason the table exists: anyone
// can type an address into a chat.
func (s *Store) LinkTreasury(ctx context.Context, p LinkTreasuryParams) (Treasury, error) {
	var verifiedBy any
	if p.VerifiedBy != 0 {
		verifiedBy = int64(p.VerifiedBy)
	}
	var contract any
	if p.Contract != "" {
		contract = p.Contract
	}

	t, err := scanTreasury(s.pool.QueryRow(ctx, `
		INSERT INTO org_treasuries (
			org_id, kind, address, label, contract_id,
			low_threshold, medium_threshold, high_threshold, verified_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (address) DO UPDATE
		   SET label            = EXCLUDED.label,
		       low_threshold    = EXCLUDED.low_threshold,
		       medium_threshold = EXCLUDED.medium_threshold,
		       high_threshold   = EXCLUDED.high_threshold,
		       verified_by      = EXCLUDED.verified_by,
		       verified_at      = now(),
		       refreshed_at     = now()
		 -- The predicate is what stops a second org taking over an address the
		 -- first one proved. Without it, ON CONFLICT would happily move the
		 -- treasury between tenants and every payment to it would post twice.
		 WHERE org_treasuries.org_id = EXCLUDED.org_id
		RETURNING `+treasuryColumns,
		int64(p.Org), p.Kind, p.Address, p.Label, contract,
		p.Low, p.Medium, p.High, verifiedBy,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Treasury{}, fmt.Errorf("%w: %s is linked to another organisation",
			ErrNoTreasury, p.Address)
	}
	if err != nil {
		return Treasury{}, fmt.Errorf("store: link treasury %s: %w", p.Address, err)
	}
	return t, nil
}

const treasuryColumns = `
	id, org_id, kind, address, label,
	low_threshold, medium_threshold, high_threshold,
	COALESCE(contract_id, ''), verified_at, refreshed_at`

func scanTreasury(row pgx.Row) (Treasury, error) {
	var t Treasury
	err := row.Scan(&t.ID, &t.Org, &t.Kind, &t.Address, &t.Label,
		&t.Low, &t.Medium, &t.High, &t.Contract, &t.VerifiedAt, &t.RefreshedAt)
	return t, err
}

// Treasury returns one linked treasury.
func (s *Store) Treasury(ctx context.Context, org ledger.OrgID, id TreasuryID) (Treasury, error) {
	t, err := scanTreasury(s.pool.QueryRow(ctx,
		`SELECT `+treasuryColumns+` FROM org_treasuries WHERE id = $1 AND org_id = $2`,
		int64(id), int64(org)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Treasury{}, fmt.Errorf("%w: %d", ErrNoTreasury, id)
	}
	if err != nil {
		return Treasury{}, fmt.Errorf("store: treasury %d: %w", id, err)
	}
	return t, nil
}

// TreasuryByAddress returns a linked treasury by its account.
func (s *Store) TreasuryByAddress(
	ctx context.Context, org ledger.OrgID, address string,
) (Treasury, error) {
	t, err := scanTreasury(s.pool.QueryRow(ctx,
		`SELECT `+treasuryColumns+` FROM org_treasuries WHERE address = $1 AND org_id = $2`,
		address, int64(org)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Treasury{}, fmt.Errorf("%w: %s", ErrNoTreasury, address)
	}
	if err != nil {
		return Treasury{}, fmt.Errorf("store: treasury %s: %w", address, err)
	}
	return t, nil
}

// Treasuries lists an org's linked treasuries, oldest first.
func (s *Store) Treasuries(ctx context.Context, org ledger.OrgID) ([]Treasury, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+treasuryColumns+` FROM org_treasuries WHERE org_id = $1 ORDER BY id`,
		int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: treasuries: %w", err)
	}
	defer rows.Close()

	var out []Treasury
	for rows.Next() {
		t, err := scanTreasury(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan treasury: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveSignerSet replaces the cached signer set for a treasury.
//
// Replaces rather than merges, and in one transaction. A signer removed on
// chain has to disappear here too, and a merge that only ever adds would keep
// naming an ex-member as someone the proposal is waiting on.
func (s *Store) SaveSignerSet(
	ctx context.Context, org ledger.OrgID, id TreasuryID,
	signers map[string]int32, low, medium, high int32,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The org predicate is on the UPDATE, not on a prior read, so a treasury id
	// belonging to another tenant writes nothing rather than being caught by a
	// check somebody could later reorder.
	var found TreasuryID
	err = tx.QueryRow(ctx, `
		UPDATE org_treasuries
		   SET low_threshold = $3, medium_threshold = $4, high_threshold = $5,
		       refreshed_at = now()
		 WHERE id = $1 AND org_id = $2
		RETURNING id`,
		int64(id), int64(org), low, medium, high).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNoTreasury, id)
	}
	if err != nil {
		return fmt.Errorf("store: refresh treasury %d: %w", id, err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM treasury_signers WHERE treasury_id = $1`, int64(id)); err != nil {
		return fmt.Errorf("store: clear signers: %w", err)
	}
	for address, weight := range signers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO treasury_signers (treasury_id, address, weight)
			VALUES ($1, $2, $3)`, int64(id), address, weight); err != nil {
			return fmt.Errorf("store: save signer %s: %w", address, err)
		}
	}
	return tx.Commit(ctx)
}

// CachedSignerSet returns the signer set as it was last read from the network.
//
// Named for what it is. A caller that wants to know whether an envelope is
// authorised must ask the network; this answers the different, cheaper question
// of who the bot should tell the channel it is waiting on.
func (s *Store) CachedSignerSet(
	ctx context.Context, org ledger.OrgID, id TreasuryID,
) (map[string]int32, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts.address, ts.weight
		  FROM treasury_signers ts
		  JOIN org_treasuries t ON t.id = ts.treasury_id
		 WHERE ts.treasury_id = $1 AND t.org_id = $2`,
		int64(id), int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: cached signer set: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int32)
	for rows.Next() {
		var address string
		var weight int32
		if err := rows.Scan(&address, &weight); err != nil {
			return nil, fmt.Errorf("store: scan signer: %w", err)
		}
		out[address] = weight
	}
	return out, rows.Err()
}
