package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// batchFixture is an org with a saved recipient to resolve against.
type batchFixture struct {
	svc     *Service
	scope   Scope
	org     store.Org
	adaAddr string
}

func newBatchFixture(t *testing.T, name string) *batchFixture {
	t.Helper()
	ctx := context.Background()

	f := newTreasuryFixture(t, name)
	ada := keypair.MustRandom().Address()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO beneficiaries (org_id, owner_ref, label, address)
		VALUES ($1, $2, $3, $4)`,
		int64(f.scope.Org), f.scope.OwnerRef, "Ada", ada); err != nil {
		t.Fatalf("save beneficiary: %v", err)
	}
	return &batchFixture{svc: f.svc, scope: f.scope, adaAddr: ada}
}

func draftRow(line int, amount, destination, memo string) connector.DraftRow {
	return connector.DraftRow{
		Line:            line,
		AmountText:      connector.Wrap(amount),
		DestinationText: connector.Wrap(destination),
		MemoText:        connector.Wrap(memo),
	}
}

func TestResolveBatchDoesTheArithmeticItself(t *testing.T) {
	f := newBatchFixture(t, "batch happy")
	bo := keypair.MustRandom().Address()

	batch, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{
			draftRow(2, "250", "Ada", "August"),
			draftRow(3, "1,000.50", bo, ""),
			// The label match is case-insensitive, because a spreadsheet's
			// capitalisation is nobody's intent.
			draftRow(4, "0.0000001", "ada", ""),
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(batch.Rows) != 3 {
		t.Fatalf("%d rows", len(batch.Rows))
	}
	if batch.Rows[0].Destination != f.adaAddr || batch.Rows[0].DestinationLabel != "Ada" {
		t.Errorf("row 0 = %+v", batch.Rows[0])
	}
	if batch.Rows[1].Destination != bo {
		t.Errorf("row 1 destination = %q", batch.Rows[1].Destination)
	}
	// The total is what a person approves. Computed in stroops from the
	// resolved rows, never from anything the connector said.
	want := money.MustParse("250") + money.MustParse("1000.50") + money.Stroops(1)
	if batch.Total != want {
		t.Fatalf("total = %s, want %s", batch.Total, want)
	}
	// The connector's own words survive for display and audit, and are not
	// what was paid.
	if batch.Rows[1].SaidAmount != "1,000.50" {
		t.Errorf("said amount = %q", batch.Rows[1].SaidAmount)
	}
}

// TestOneBadRowFailsAllOfThem is the decision this file is arranged around.
//
// Paying the rows that parsed is wrong in a way with no symptom: a payroll
// sheet somebody is midway through editing pays 199 of 200, the approval
// screen showed a total that looked right, and the person left out finds out a
// week later. Failing whole is recoverable; paying partially is not.
func TestOneBadRowFailsAllOfThem(t *testing.T) {
	f := newBatchFixture(t, "batch one bad")

	_, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{
			draftRow(2, "250", "Ada", ""),
			draftRow(3, "five hundred", "Ada", ""),
			draftRow(4, "100", "Ada", ""),
		},
	})
	if !errors.Is(err, ErrBatchUnreadable) {
		t.Fatalf("error = %v, want ErrBatchUnreadable", err)
	}
	if !strings.Contains(err.Error(), "row 3") {
		t.Errorf("the error does not name the bad row: %v", err)
	}
}

// TestEveryBadRowIsNamedAtOnce: reporting the first and stopping means fixing
// one cell, running again, and finding the next. For two hundred rows that is
// a long afternoon.
func TestEveryBadRowIsNamedAtOnce(t *testing.T) {
	f := newBatchFixture(t, "batch all bad")

	_, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{
			draftRow(2, "250", "Ada", ""),
			draftRow(3, "=B2*C2", "Ada", ""),
			draftRow(4, "100", "Nobody", ""),
			draftRow(5, "(50)", "Ada", ""),
			draftRow(6, "100", "GNOTANADDRESSATALLXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", ""),
		},
	})
	if err == nil {
		t.Fatal("accepted")
	}

	var batchErr *BatchError
	if !errors.As(err, &batchErr) {
		t.Fatalf("error is %T, want *BatchError", err)
	}
	if len(batchErr.Rows) != 4 {
		t.Fatalf("%d bad rows reported, want 4: %v", len(batchErr.Rows), err)
	}
	// In source order, so a person reads down their sheet rather than jumping.
	for i, want := range []int{3, 4, 5, 6} {
		if batchErr.Rows[i].Line != want {
			t.Errorf("failure %d is row %d, want %d", i, batchErr.Rows[i].Line, want)
		}
	}

	// And each says something specific enough to act on.
	joined := err.Error()
	for _, want := range []string{"formula", "not a saved recipient", "accounting", "checksum"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not explain %q:\n%s", want, joined)
		}
	}
}

// TestALabelMustMatchExactly: the chat path accepts a single substring match
// because there is a person there to be asked. Two hundred rows have nobody to
// ask, and a near-match paid two hundred times is the failure this guards.
func TestALabelMustMatchExactly(t *testing.T) {
	f := newBatchFixture(t, "batch near miss")

	_, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", "Ad", "")},
	})
	if !errors.Is(err, ErrBatchUnreadable) {
		t.Fatalf("a partial label resolved: %v", err)
	}
}

// TestAMistypedAddressIsSaidPrecisely: read as a label it would search the
// address book for something nobody named, and send a person looking in the
// wrong place.
func TestAMistypedAddressIsSaidPrecisely(t *testing.T) {
	f := newBatchFixture(t, "batch bad address")
	broken := keypair.MustRandom().Address()
	broken = broken[:len(broken)-1] + "A"

	_, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", broken, "")},
	})
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("the error does not say the address is malformed: %v", err)
	}
}

func TestABatchHasBoundsAtBothEnds(t *testing.T) {
	f := newBatchFixture(t, "batch bounds")
	ctx := context.Background()

	if _, err := f.svc.ResolveBatch(ctx, f.scope, connector.Draft{}); !errors.Is(
		err, ErrBatchEmpty,
	) {
		t.Errorf("an empty draft: %v", err)
	}

	rows := make([]connector.DraftRow, MaxBatchRows+1)
	for i := range rows {
		rows[i] = draftRow(i+1, "1", "Ada", "")
	}
	if _, err := f.svc.ResolveBatch(ctx, f.scope, connector.Draft{Rows: rows}); !errors.Is(
		err, ErrBatchTooLarge,
	) {
		t.Errorf("an oversized draft: %v", err)
	}

	// Exactly at the limit is fine, because a limit somebody cannot reach is a
	// limit set one lower than it says.
	batch, err := f.svc.ResolveBatch(ctx, f.scope, connector.Draft{Rows: rows[:MaxBatchRows]})
	if err != nil {
		t.Fatalf("a batch at the limit was refused: %v", err)
	}
	if len(batch.Rows) != MaxBatchRows {
		t.Errorf("%d rows", len(batch.Rows))
	}
}

// TestAMemoTooLongFailsRatherThanTruncating: truncating would put a different
// note on chain than the sheet says.
func TestAMemoTooLongFailsRatherThanTruncating(t *testing.T) {
	f := newBatchFixture(t, "batch memo")

	_, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", "Ada", strings.Repeat("x", 29))},
	})
	if !errors.Is(err, ErrBatchUnreadable) {
		t.Fatalf("a long memo was accepted: %v", err)
	}

	// 28 is fine.
	if _, err := f.svc.ResolveBatch(context.Background(), f.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", "Ada", strings.Repeat("x", 28))},
	}); err != nil {
		t.Fatalf("a 28-character memo was refused: %v", err)
	}
}

// TestABatchIsScopedToItsOrg: another tenant's address book must not resolve
// this one's rows.
func TestABatchIsScopedToItsOrg(t *testing.T) {
	ctx := context.Background()
	f := newBatchFixture(t, "batch scope a")
	other := newBatchFixture(t, "batch scope b")

	// A label only the first org has saved.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO beneficiaries (org_id, owner_ref, label, address)
		VALUES ($1, $2, $3, $4)`,
		int64(f.scope.Org), f.scope.OwnerRef, "Payroll",
		keypair.MustRandom().Address()); err != nil {
		t.Fatalf("save beneficiary: %v", err)
	}
	if _, err := f.svc.ResolveBatch(ctx, f.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", "Payroll", "")},
	}); err != nil {
		t.Fatalf("the owning org could not resolve its own recipient: %v", err)
	}

	_, err := f.svc.ResolveBatch(ctx, other.scope, connector.Draft{
		Rows: []connector.DraftRow{draftRow(2, "250", "Payroll", "")},
	})
	if !errors.Is(err, ErrBatchUnreadable) {
		t.Fatalf("another org's recipient resolved: %v", err)
	}
}
