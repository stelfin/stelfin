package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// Cursor is a position in a history listing.
//
// Keyset, not OFFSET. A DAO's history grows without bound, and paging by offset
// makes "page 40" a scan of everything before it — the cost grows with how far
// back someone looks, which is exactly backwards. The pair matches
// ledger_transactions_org_occurred_idx, so a page is an index range scan.
type Cursor struct {
	OccurredAt time.Time
	TxID       ledger.TxID
}

// Zero reports an unset cursor, meaning start at the newest row.
func (c Cursor) Zero() bool { return c.TxID == 0 && c.OccurredAt.IsZero() }

// HistoryFilter narrows a history listing.
type HistoryFilter struct {
	// Account limits to one account. Zero means every account in the org.
	Account ledger.AccountID
	// Asset limits to one asset. Zero means every asset.
	Asset ledger.AssetID
	// Kinds limits to particular transaction kinds. Empty means all.
	Kinds []ledger.TxKind
	// Since and Until bound occurred_at. Zero means unbounded.
	Since, Until time.Time
	// Limit caps the page. Zero uses DefaultHistoryLimit; anything above
	// MaxHistoryLimit is clamped.
	Limit int
	// After continues a previous page.
	After Cursor
}

// History page sizes.
const (
	DefaultHistoryLimit = 25
	MaxHistoryLimit     = 200
)

// HistoryRow is one line of history: a movement on one account in one asset.
//
// A journal entry can touch several accounts, so one transaction may appear as
// several rows. That is deliberate — a listing scoped to an account should show
// what happened to *that* account, not oblige the reader to work it out from a
// balanced set of postings.
type HistoryRow struct {
	TxID        ledger.TxID
	Kind        ledger.TxKind
	OccurredAt  time.Time
	ExternalRef string
	Account     ledger.AccountID
	AccountName string
	Asset       ledger.AssetID
	AssetCode   string
	Amount      money.Stroops
}

// History returns a page of an org's history, newest first, with the cursor to
// continue from.
//
// A zero cursor comes back when the page is the last one.
func (s *Store) History(
	ctx context.Context, org ledger.OrgID, f HistoryFilter,
) ([]HistoryRow, Cursor, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = DefaultHistoryLimit
	case limit > MaxHistoryLimit:
		limit = MaxHistoryLimit
	}

	// Built by appending, so every value is a parameter and none of the
	// filter's contents ever reach the query text.
	args := []any{int64(org)}
	where := []string{"t.org_id = $1"}

	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Account != 0 {
		add("e.account_id = $%d", int64(f.Account))
	}
	if f.Asset != 0 {
		add("e.asset_id = $%d", int16(f.Asset))
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		add("t.kind = ANY($%d)", kinds)
	}
	if !f.Since.IsZero() {
		add("t.occurred_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("t.occurred_at <= $%d", f.Until)
	}
	if !f.After.Zero() {
		// Row-value comparison, matching the index's own ordering. Written as a
		// tuple rather than as (a < x) OR (a = x AND b < y) because the tuple
		// form is what lets Postgres seek straight to the position.
		args = append(args, f.After.OccurredAt, int64(f.After.TxID))
		where = append(where,
			fmt.Sprintf("(t.occurred_at, t.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1) // one extra, to learn whether more remain

	query := `
		SELECT t.id, t.kind, t.occurred_at, COALESCE(t.external_ref, ''),
		       e.account_id, a.name, e.asset_id, s.code, e.amount
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.transaction_id
		  JOIN ledger_accounts a ON a.id = e.account_id
		  JOIN assets s ON s.id = e.asset_id
		 WHERE ` + strings.Join(where, "\n		   AND ") + `
		 ORDER BY t.occurred_at DESC, t.id DESC, e.id DESC
		 LIMIT $` + fmt.Sprint(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, Cursor{}, fmt.Errorf("store: history: %w", err)
	}
	defer rows.Close()

	out := make([]HistoryRow, 0, limit)
	for rows.Next() {
		var r HistoryRow
		if err := rows.Scan(&r.TxID, &r.Kind, &r.OccurredAt, &r.ExternalRef,
			&r.Account, &r.AccountName, &r.Asset, &r.AssetCode, &r.Amount); err != nil {
			return nil, Cursor{}, fmt.Errorf("store: scan history row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, Cursor{}, fmt.Errorf("store: iterate history: %w", err)
	}

	if len(out) <= limit {
		return out, Cursor{}, nil
	}
	out = out[:limit]
	last := out[len(out)-1]
	return out, Cursor{OccurredAt: last.OccurredAt, TxID: last.TxID}, nil
}
