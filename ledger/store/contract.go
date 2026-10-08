package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

var (
	// ErrContractNotAllowed reports a contract this workspace has not allowed.
	ErrContractNotAllowed = errors.New("store: that contract is not allowed here")
	// ErrContractExists reports a label or address already allowed here.
	ErrContractExists = errors.New("store: that contract is already allowed here")
)

// AllowedContract is a third-party contract a workspace may call, and the code
// it was running when it was allowed.
type AllowedContract struct {
	ID         int64
	Org        ledger.OrgID
	Label      string
	Address    string
	Executable string
	WasmHash   string
	AddedAt    time.Time
}

const allowedColumns = `id, org_id, label, address, executable, wasm_hash, added_at`

func scanAllowed(row pgx.Row) (AllowedContract, error) {
	var c AllowedContract
	err := row.Scan(&c.ID, &c.Org, &c.Label, &c.Address, &c.Executable, &c.WasmHash, &c.AddedAt)
	return c, err
}

// AllowContract records a contract as callable from this workspace.
func (s *Store) AllowContract(ctx context.Context, c AllowedContract, by MemberID) (AllowedContract, error) {
	out, err := scanAllowed(s.pool.QueryRow(ctx, `
		INSERT INTO allowed_contracts (org_id, label, address, executable, wasm_hash, added_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+allowedColumns,
		int64(c.Org), c.Label, c.Address, c.Executable, c.WasmHash, int64(by)))
	if isUniqueViolation(err) {
		return AllowedContract{}, fmt.Errorf("%w: %s", ErrContractExists, c.Label)
	}
	if err != nil {
		return AllowedContract{}, fmt.Errorf("store: allow contract: %w", err)
	}
	return out, nil
}

// AllowedContractByLabel returns a workspace's allowed contract by its label.
func (s *Store) AllowedContractByLabel(ctx context.Context, org ledger.OrgID, label string) (AllowedContract, error) {
	return s.allowedWhere(ctx, `org_id = $1 AND label = $2`, int64(org), label)
}

// AllowedContractByAddress returns a workspace's allowed contract by address.
func (s *Store) AllowedContractByAddress(ctx context.Context, org ledger.OrgID, address string) (AllowedContract, error) {
	return s.allowedWhere(ctx, `org_id = $1 AND address = $2`, int64(org), address)
}

func (s *Store) allowedWhere(ctx context.Context, where string, args ...any) (AllowedContract, error) {
	c, err := scanAllowed(s.pool.QueryRow(ctx,
		`SELECT `+allowedColumns+` FROM allowed_contracts WHERE removed_at IS NULL AND `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return AllowedContract{}, ErrContractNotAllowed
	}
	if err != nil {
		return AllowedContract{}, fmt.Errorf("store: allowed contract: %w", err)
	}
	return c, nil
}

// AllowedContracts lists a workspace's allowed contracts, by label.
func (s *Store) AllowedContracts(ctx context.Context, org ledger.OrgID) ([]AllowedContract, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+allowedColumns+` FROM allowed_contracts
		 WHERE org_id = $1 AND removed_at IS NULL ORDER BY label`, int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: list allowed contracts: %w", err)
	}
	defer rows.Close()
	var out []AllowedContract
	for rows.Next() {
		c, err := scanAllowed(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan allowed contract: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DisallowContract removes a contract from a workspace's allowlist.
func (s *Store) DisallowContract(ctx context.Context, org ledger.OrgID, label string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE allowed_contracts SET removed_at = now()
		 WHERE org_id = $1 AND label = $2 AND removed_at IS NULL`, int64(org), label)
	if err != nil {
		return fmt.Errorf("store: disallow contract: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrContractNotAllowed, label)
	}
	return nil
}
