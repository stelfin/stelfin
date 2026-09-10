// Package connector is how a DAO reaches the tools it already uses.
//
// A community keeps its payroll in a spreadsheet, its decisions in a document,
// its prices in some service nobody here has heard of. The thesis is that all
// of it should be reachable from the same chat window as the treasury. The
// danger is the same sentence read backwards: everything a community reaches
// becomes something that can reach the treasury.
//
// So this package is built around one rule, and the rest follows from it.
//
// # Nothing a connector returns is ever a decision
//
// A connector returns text. Not amounts, not addresses, not accounts — text.
// stelfin re-parses and re-resolves every part of it against its own tables,
// exactly as it does with the language model's output, because the two are the
// same kind of input: something outside the trust boundary proposing what
// should happen.
//
// That is why this package imports neither settlement nor txnbuild, and an
// import-graph test enforces it. A connector that could build a transaction
// would be a connector that could decide one, and no amount of care downstream
// recovers from that.
//
// # Reader or Proposer, never both
//
// A Reader observes: it returns rows of strings and cannot ask for anything to
// happen. A Proposer suggests a payment, in text, which a human then approves
// through the same confirmation screen as any other. A connector that could do
// both would let a read — the cheap, frequently-granted capability — become a
// write by way of a code path somebody forgot to check.
package connector

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotGranted reports a connector the DAO has not authorised.
	ErrNotGranted = errors.New("connector: this workspace has not granted that connector")

	// ErrBothKinds reports a connector that observes and proposes.
	ErrBothKinds = errors.New("connector: a connector reads or proposes, never both")

	// ErrMalformedRow reports data this package refuses to interpret.
	//
	// Refused rather than skipped. A spreadsheet with one unreadable row is a
	// spreadsheet somebody is still editing, and paying the other 199 rows
	// while quietly dropping one is the failure mode with no symptom.
	ErrMalformedRow = errors.New("connector: a row cannot be read")
)

// Untrusted holds a value that came from outside stelfin.
//
// The unexported field is the whole mechanism: an Untrusted[string] cannot be
// passed where a string is wanted, cannot be concatenated into a query, and
// cannot be handed to anything that acts. Getting the value out means calling
// Unwrap, which is one word a reviewer can search for and a diff will show.
//
// It buys a compile-time answer to a question that is otherwise asked in a code
// review and answered wrongly once: was this treated as data?
type Untrusted[T any] struct {
	value T
}

// Wrap marks a value as having come from outside.
func Wrap[T any](value T) Untrusted[T] { return Untrusted[T]{value: value} }

// Unwrap returns the value, and is deliberately the only way to.
//
// Every call site is a place where something from outside is about to be used.
// That is not a reason to avoid it — it is a reason for it to be visible.
func (u Untrusted[T]) Unwrap() T { return u.value }

// Kind says what a connector is allowed to do.
type Kind string

const (
	// KindReader observes and cannot ask for anything to happen.
	KindReader Kind = "reader"
	// KindProposer suggests a payment a human then approves.
	KindProposer Kind = "proposer"
)

// Descriptor is what a connector says about itself.
type Descriptor struct {
	// ID is the name a grant is recorded against, matching the symbol in the
	// on-chain registry.
	ID string
	// Kind is what it may do.
	Kind Kind
	// Capabilities are the named things it offers, and the digest over them is
	// what the registry pins.
	Capabilities []string
}

// Observation is what a Reader returns.
//
// Rows of strings and nothing else. No amounts, no addresses, no account
// identifiers — not because a spreadsheet cannot contain them, but because a
// type that carried them would invite something downstream to use them without
// resolving them first.
type Observation struct {
	// Columns are the headings, as the source gave them.
	Columns []Untrusted[string]
	// Rows are the cells, in the order the source returned them.
	Rows [][]Untrusted[string]
	// ReadAt is when this was read, so a stale observation can be recognised.
	ReadAt time.Time
	// Truncated reports that the source had more than was asked for.
	//
	// Carried rather than hidden: a payroll sheet silently cut at 100 rows is a
	// hundred people paid and the rest not, with nothing saying so.
	Truncated bool
}

// Draft is what a Proposer returns: a payment, as text, for a human to approve.
//
// Every field is text and every field is untrusted. The backend parses the
// amount with its own parser, resolves the destination against its own address
// book, and refuses anything it cannot. A connector that returned a resolved
// address would be a connector deciding where money goes.
type Draft struct {
	Rows []DraftRow
	// ReadAt is when the source was read.
	ReadAt time.Time
	// Truncated reports that the source had more rows than were asked for.
	Truncated bool
}

// DraftRow is one proposed payment.
type DraftRow struct {
	// Line is where in the source this came from, so an error can name it.
	//
	// One-based, because it is shown to a person looking at a spreadsheet and
	// spreadsheets start at one.
	Line int

	AmountText      Untrusted[string]
	DestinationText Untrusted[string]
	MemoText        Untrusted[string]
}

// Reader observes something outside stelfin.
type Reader interface {
	Describe() Descriptor
	Read(ctx context.Context, request Request) (Observation, error)
}

// Proposer suggests payments for a human to approve.
type Proposer interface {
	Describe() Descriptor
	Propose(ctx context.Context, request Request) (Draft, error)
}

// Request is what a connector is being asked for.
//
// Deliberately thin. A connector receives a selector it understands and a limit
// it must respect; it does not receive an org id, a member, a treasury or a
// balance. Nothing here would help a hostile connector decide what to return,
// because nothing here tells it anything about the money.
type Request struct {
	// Selector names what to read, in whatever vocabulary the connector uses —
	// a sheet range, a document id, a query.
	Selector string
	// Limit bounds how many rows may come back.
	Limit int
}

// CheckKind refuses a connector that both reads and proposes.
//
// Checked at registration rather than trusted from the descriptor, because the
// descriptor is the connector's own claim about itself and the interfaces are
// the fact.
func CheckKind(c any) error {
	_, reads := c.(Reader)
	_, proposes := c.(Proposer)

	switch {
	case reads && proposes:
		return fmt.Errorf("%w: %T does both", ErrBothKinds, c)
	case !reads && !proposes:
		return fmt.Errorf("connector: %T is neither a Reader nor a Proposer", c)
	}

	// And the claim has to match the fact, or a grant recorded against
	// "reader" would be authorising something that proposes.
	described := describe(c)
	if reads && described.Kind != KindReader {
		return fmt.Errorf("%w: %T reads but calls itself %q", ErrBothKinds, c, described.Kind)
	}
	if proposes && described.Kind != KindProposer {
		return fmt.Errorf("%w: %T proposes but calls itself %q", ErrBothKinds, c, described.Kind)
	}
	return nil
}

func describe(c any) Descriptor {
	if r, ok := c.(Reader); ok {
		return r.Describe()
	}
	if p, ok := c.(Proposer); ok {
		return p.Describe()
	}
	return Descriptor{}
}
