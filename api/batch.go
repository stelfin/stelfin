package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Turning what a connector proposed into payments.
//
// A connector returns text. This is where that text stops being text, and it is
// the only place: the amount is parsed by stelfin's own parser, the destination
// resolved against stelfin's own address book, and anything that will not
// resolve stops the batch.
//
// # Why one bad row fails all of them
//
// The tempting behaviour is to pay the rows that parsed and report the rest.
// That is wrong in a way that has no symptom: a payroll sheet somebody is
// midway through editing pays 199 of 200 people, the approval screen showed a
// total that looked right, and the person left out finds out a week later.
//
// Failing whole is recoverable — fix the sheet, run it again. Paying partially
// is not, because the payments that went through cannot be taken back.
//
// # Why every bad row is named
//
// Reporting the first failure and stopping means a person fixes one cell, runs
// it again, and finds the next. For two hundred rows that is a long afternoon.
// Every bad row is collected and reported together.

var (
	// ErrBatchUnreadable reports a draft with rows this cannot interpret.
	ErrBatchUnreadable = errors.New("api: this batch has rows that cannot be paid")

	// ErrBatchEmpty reports a draft with nothing in it.
	//
	// Its own error because it is usually a selector pointing at the wrong
	// range rather than a deliberate request to pay nobody.
	ErrBatchEmpty = errors.New("api: this batch has no rows")

	// ErrBatchTooLarge reports a draft beyond what one transaction can carry.
	ErrBatchTooLarge = errors.New("api: this batch has more rows than one transaction can hold")
)

// MaxBatchRows is how many payments one batch may carry.
//
// Stellar allows 100 operations per transaction. Leaving headroom for the
// sponsorship or trustline operations a batch may need means not spending all
// of it on payments — a batch that builds and then cannot have an operation
// added is a batch that fails at the last step for a reason nobody would guess.
const MaxBatchRows = 90

// BatchRow is one payment, resolved.
type BatchRow struct {
	// Line is where in the source it came from, so an audit trail can point at
	// a spreadsheet row.
	Line int

	Amount      money.Stroops
	Destination string
	// DestinationLabel is what to show a person: their saved label where there
	// is one, the address otherwise.
	DestinationLabel string
	Memo             string

	// SaidAmount and SaidDestination are the connector's own words, kept for
	// display and for the audit trail. Never used to build anything.
	SaidAmount      string
	SaidDestination string
}

// Batch is a resolved draft, ready to be described and approved.
type Batch struct {
	Rows  []BatchRow
	Total money.Stroops
	// Truncated reports that the source had more rows than were read.
	Truncated bool
}

// RowError is one row that could not be paid.
type RowError struct {
	Line   int
	Reason string
}

// BatchError carries every bad row at once.
type BatchError struct {
	Rows []RowError
}

func (e *BatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d row(s) cannot be paid:", len(e.Rows))
	for _, r := range e.Rows {
		fmt.Fprintf(&b, "\n  row %d: %s", r.Line, r.Reason)
	}
	return b.String()
}

// Is makes errors.Is(err, ErrBatchUnreadable) work on a collected failure.
func (e *BatchError) Is(target error) bool { return target == ErrBatchUnreadable }

// ResolveBatch turns a connector's draft into payments, or refuses it entirely.
func (s *Service) ResolveBatch(
	ctx context.Context, scope Scope, draft connector.Draft,
) (*Batch, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if len(draft.Rows) == 0 {
		return nil, ErrBatchEmpty
	}
	if len(draft.Rows) > MaxBatchRows {
		return nil, fmt.Errorf("%w: %d rows, and one transaction holds %d",
			ErrBatchTooLarge, len(draft.Rows), MaxBatchRows)
	}

	batch := &Batch{Truncated: draft.Truncated}
	var failures []RowError

	// Every row is attempted even after one fails, so the report is complete.
	// Stopping at the first would mean a person fixes one cell, runs it again,
	// and finds the next — for two hundred rows, a long afternoon.
	for _, row := range draft.Rows {
		resolved, err := s.resolveRow(ctx, scope, row)
		if err != nil {
			failures = append(failures, RowError{Line: row.Line, Reason: err.Error()})
			continue
		}
		batch.Rows = append(batch.Rows, resolved)
	}

	if len(failures) > 0 {
		sort.Slice(failures, func(i, j int) bool { return failures[i].Line < failures[j].Line })
		return nil, &BatchError{Rows: failures}
	}

	// Summed after every row resolved, in stroops, with an overflow check. The
	// total is what a person actually approves — a wrong one is a wrong
	// approval however right the rows are.
	for _, row := range batch.Rows {
		next := batch.Total + row.Amount
		if next < batch.Total {
			return nil, fmt.Errorf("%w: the total overflows", ErrBatchUnreadable)
		}
		batch.Total = next
	}
	if batch.Total <= 0 {
		return nil, fmt.Errorf("%w: the total is not positive", ErrBatchUnreadable)
	}
	return batch, nil
}

// resolveRow parses and resolves one row, using nothing the connector said as
// an answer.
func (s *Service) resolveRow(
	ctx context.Context, scope Scope, row connector.DraftRow,
) (BatchRow, error) {
	text, err := connector.NormalizeTabularAmount(row.AmountText)
	if err != nil {
		return BatchRow{}, err
	}
	amount, err := money.Parse(text)
	if err != nil {
		// Unreachable through NormalizeTabularAmount, which is the point of
		// having both: this is the second reading of the same cell, and the two
		// disagreeing is a bug rather than a bad row.
		return BatchRow{}, fmt.Errorf("the amount %q could not be read a second time: %w",
			text, err)
	}

	destination, label, err := s.resolveDestination(ctx, scope, row.DestinationText.Unwrap())
	if err != nil {
		return BatchRow{}, err
	}

	memo := strings.TrimSpace(row.MemoText.Unwrap())
	if len([]rune(memo)) > 28 {
		// Truncating would put a different note on chain than the sheet says.
		return BatchRow{}, fmt.Errorf(
			"the memo is %d characters and a Stellar memo holds 28", len([]rune(memo)))
	}

	return BatchRow{
		Line:             row.Line,
		Amount:           amount,
		Destination:      destination,
		DestinationLabel: label,
		Memo:             memo,
		SaidAmount:       row.AmountText.Unwrap(),
		SaidDestination:  row.DestinationText.Unwrap(),
	}, nil
}

// resolveDestination turns a cell into an address this org can pay.
//
// An address is taken as itself; anything else is looked up in the org's own
// address book. A label that matches nothing is a failure rather than a guess —
// the whole reason the address book is human-written is that nothing else gets
// to decide where money goes.
func (s *Service) resolveDestination(
	ctx context.Context, scope Scope, text string,
) (address, label string, err error) {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == "":
		return "", "", errors.New("it has no destination")
	case strkey.IsValidEd25519PublicKey(trimmed):
		return trimmed, trimmed, nil
	case strings.HasPrefix(trimmed, "G"):
		// Looks like an address and is not one. Said precisely, because a
		// mistyped address read as a label would search the address book for
		// something nobody named and report "not found", which sends a person
		// looking in the wrong place.
		return "", "", fmt.Errorf(
			"%q looks like a Stellar address but its checksum is wrong", clipCell(trimmed))
	}

	var found, foundLabel string
	err = s.pool.QueryRow(ctx, `
		SELECT address, label FROM beneficiaries
		 WHERE org_id = $1 AND owner_ref = $2 AND lower(label) = lower($3)`,
		int64(scope.Org), scope.OwnerRef, trimmed,
	).Scan(&found, &foundLabel)
	if err != nil {
		// Exact matches only. The chat path accepts a single substring match
		// because there is a person there to be asked; two hundred rows have
		// nobody to ask, and a near-match paid two hundred times is the
		// failure this whole file is arranged against.
		return "", "", fmt.Errorf(
			"%q is not a saved recipient in this workspace", clipCell(trimmed))
	}
	return found, foundLabel, nil
}

// clipCell bounds what an error quotes back from a cell.
func clipCell(s string) string {
	const limit = 40
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// ProposeBatch puts a resolved batch to the treasury's signers.
//
// A batch is a proposal, not a second kind of approval. That is a deliberate
// reuse: a payroll run is exactly the case where a DAO wants several people to
// look before money moves, and building it a separate confirmation path would
// mean two screens applying different rules to the same envelope — with the
// weaker one reachable.
func (s *Service) ProposeBatch(
	ctx context.Context, scope Scope, p ProposeBatchParams,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}

	batch, err := s.ResolveBatch(ctx, scope, p.Draft)
	if err != nil {
		return nil, err
	}

	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, p.Treasury)
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryClassic {
		return nil, fmt.Errorf("api: %s is a contract account; its authorisation is not signatures",
			treasury.Address)
	}

	lines := make([]settlement.PayrollLine, 0, len(batch.Rows))
	for _, row := range batch.Rows {
		lines = append(lines, settlement.PayrollLine{
			To: row.Destination, Asset: s.cfg.Asset, Amount: row.Amount,
		})
	}
	ops, err := settlement.Payroll(treasury.Address, lines)
	if err != nil {
		return nil, err
	}

	tx, err := s.settle.Build(ctx, settlement.BuildRequest{
		Source:     treasury.Address,
		Operations: ops,
		Memo:       memoOf(p.Memo),
		Timeout:    org.ProposalTTL,
	})
	if err != nil {
		return nil, err
	}

	// Described by the strict renderer before it is stored. A batch this
	// deployment cannot show as a batch must not reach a screen that will
	// summarise it as one — and the total is the whole thing a person reads.
	described, err := s.settle.DescribeBatch(tx)
	if err != nil {
		return nil, err
	}
	if described.Total != batch.Total {
		// The envelope and the resolved rows disagree about how much is being
		// spent. Unreachable through the code above, which is exactly why it is
		// checked: the two numbers come from different places and only one of
		// them is what the network will do.
		return nil, fmt.Errorf(
			"%w: the envelope totals %s and the rows total %s",
			ErrBatchUnreadable, described.Total, batch.Total)
	}

	canonical := described.Description.Canonical()
	digest := sha256.Sum256([]byte(canonical))
	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash batch: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode batch: %w", err)
	}

	proposal, err := s.store.CreateProposal(ctx, store.CreateProposalParams{
		Org:         scope.Org,
		Treasury:    treasury.ID,
		Kind:        "payroll",
		XDR:         envelope,
		Hash:        hash,
		SourceSeq:   tx.SequenceNumber(),
		Description: canonical,
		Digest:      digest[:],
		CreatedBy:   p.CreatedBy,
		ExpiresAt:   time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	})
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, scope, proposal, treasury, tx, described.Description)
}

// ProposeBatchParams describes a batch to put to the signers.
type ProposeBatchParams struct {
	Treasury  store.TreasuryID
	Draft     connector.Draft
	Memo      string
	CreatedBy store.MemberID
}
