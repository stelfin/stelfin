// Package store is the only place a database query is written.
//
// The ledger engine next door owns posting and the invariants that protect it.
// Everything else — orgs, members, identities, addresses, pending transactions,
// saved recipients, history — lives here, and the reason is tenancy.
//
// Before this package existed, hand-written SQL sat in six others: the HTTP
// layer, the intent resolver, the ingestion worker. That is six places for a
// query to be written without an org predicate, and the symptom of forgetting
// one is not an error but another tenant's data. Collecting them makes the
// omission visible in review and testable in one place — see
// TestNoCrossOrgReads, which hands every read method one org's id and another
// org's row id and requires it to return nothing.
//
// Two mechanisms back that up. Every method takes a ledger.OrgID immediately
// after the context, even where a row id already determines the org, so a
// leaked id reads as absent rather than as someone else's. And on the write
// side a deferred trigger refuses any transaction spanning two orgs, which is
// the guarantee that survives a bug in this package.
//
// Postgres row-level security was considered and rejected. It needs SET LOCAL
// per request; most reads here are single statements outside an explicit
// transaction, where there is nothing for SET LOCAL to scope to, and pgxpool
// hands the connection back between statements. A forgotten SET LOCAL under RLS
// fails *open*, against whatever org the previous request set — worse than no
// RLS at all.
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stelfin/stelfin/ledger"
)

// Store is a handle on stelfin's database.
type Store struct {
	pool   *pgxpool.Pool
	ledger *ledger.Store
}

// New returns a Store backed by pool. The caller retains ownership of pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, ledger: ledger.New(pool)}
}

// Pool exposes the underlying pool.
//
// Present for the places that genuinely need their own transaction — the
// enrollment finalisation, which must create an account and track its address
// atomically. Not an invitation to write queries elsewhere.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ledger exposes the posting engine for the operations that are purely
// accounting and carry no tenancy question of their own, such as registering an
// asset.
func (s *Store) Ledger() *ledger.Store { return s.ledger }
