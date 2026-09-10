package ingestion

import (
	"context"
	"time"

	"github.com/stelfin/stelfin/internal/money"
)

// A source of things that happened.
//
// There are two, and they see different worlds. Horizon reports classic
// operations; Soroban RPC reports contract events. A treasury held in a
// contract produces no Horizon payment operations at all, so a deployment
// watching only the first would report a contract-custody DAO's balance as
// permanently zero — and be quietly, permanently wrong rather than visibly
// broken.
//
// The two are kept behind one interface because everything after the fetch is
// identical: the same idempotency, the same cursor discipline, the same
// double-entry posting. What differs is only where the records come from.

// Record is one thing that happened, in the terms the ledger posts.
type Record struct {
	// Cursor is where to resume after this record. Saved only once the record
	// has been applied and committed.
	Cursor string
	// ID makes a replay harmless. Derived from the chain's own identifier for
	// the event, so the same record produces the same key however many times it
	// is fetched.
	ID string
	// TxHash ties a ledger entry back to something an auditor can look up.
	TxHash string
	// OccurredAt is the ledger close time — the chain's "when", not ours. It is
	// stable across replays, which is what lets the idempotency fingerprint
	// match on the second pass.
	OccurredAt time.Time

	// Movements are the value changes, usually one.
	Movements []Movement

	// Closed names an account that stopped existing.
	//
	// Carried separately from Movements because a merge moves a balance the
	// operation does not state — Horizon reports who merged into whom and not
	// how much — so this says the fact that can be known rather than inventing
	// the one that cannot.
	Closed string
}

// Movement is value going from somewhere to somewhere.
//
// Either end may be empty: a payment out of a tracked address to a stranger has
// no tracked destination, and that is the ordinary case rather than an error.
type Movement struct {
	From string
	To   string

	// The asset, in Horizon's vocabulary. "native" needs no code or issuer.
	AssetType   string
	AssetCode   string
	AssetIssuer string

	Amount money.Stroops
}

// Source produces records in order, from a cursor.
//
// Fetch returns at most limit records. An empty cursor means "from the
// beginning", and a short page means the source has caught up — which is the
// signal Run uses to stop hammering it.
type Source interface {
	// Name is the cursor row this source tracks, so two sources cannot
	// overwrite each other's position.
	Name() string
	Fetch(ctx context.Context, cursor string, limit uint) ([]Record, error)
}
