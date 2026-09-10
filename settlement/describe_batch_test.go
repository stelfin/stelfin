package settlement

import (
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

func batchTx(t *testing.T, ops ...txnbuild.Operation) *txnbuild.Transaction {
	t.Helper()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: goldenSource, Sequence: 41,
		},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations:           ops,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tx
}

func pay(to, amount string) *txnbuild.Payment {
	return &txnbuild.Payment{Destination: to, Amount: amount, Asset: usdc()}
}

func TestABatchTotalsItsRows(t *testing.T) {
	c := goldenClient(t)
	a, b := keypair.MustRandom().Address(), keypair.MustRandom().Address()

	got, err := c.DescribeBatch(batchTx(t,
		pay(a, "250"), pay(b, "1000.50"), pay(a, "0.0000001")))
	if err != nil {
		t.Fatalf("describe batch: %v", err)
	}

	if len(got.Payments) != 3 {
		t.Fatalf("%d payments", len(got.Payments))
	}
	want := money.MustParse("250") + money.MustParse("1000.50") + money.Stroops(1)
	if got.Total != want {
		t.Fatalf("total = %s, want %s", got.Total, want)
	}
	if got.AssetCode != "USDC" {
		t.Errorf("asset = %q", got.AssetCode)
	}
	if got.Source != goldenSource {
		t.Errorf("source = %q", got.Source)
	}
	// The canonical description is still available, so the page can compare
	// bytes exactly as every other screen does.
	if got.Description == nil || got.Description.Canonical() == "" {
		t.Error("the batch carries no canonical description")
	}
}

// TestOneNonPaymentIsRefused: a trustline change hidden among ninety payments
// is invisible to somebody reading a total, and the total says nothing about
// it.
func TestOneNonPaymentIsRefused(t *testing.T) {
	c := goldenClient(t)
	a := keypair.MustRandom().Address()

	_, err := c.DescribeBatch(batchTx(t,
		pay(a, "250"),
		&txnbuild.ChangeTrust{Line: usdc().MustToChangeTrustAsset(), Limit: "1000"},
		pay(a, "250")))
	if !errors.Is(err, ErrNotABatch) {
		t.Fatalf("error = %v, want ErrNotABatch", err)
	}
	if !strings.Contains(err.Error(), "payments only") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestTwoAssetsCannotShareATotal: "90,000" across two assets is not wrong so
// much as meaningless.
func TestTwoAssetsCannotShareATotal(t *testing.T) {
	c := goldenClient(t)
	a := keypair.MustRandom().Address()

	_, err := c.DescribeBatch(batchTx(t,
		pay(a, "250"),
		&txnbuild.Payment{Destination: a, Amount: "250", Asset: txnbuild.NativeAsset{}}))
	if !errors.Is(err, ErrNotABatch) {
		t.Fatalf("error = %v, want ErrNotABatch", err)
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Errorf("the error does not explain the total: %v", err)
	}
}

// TestAnOperationWithItsOwnSourceIsRefused: it spends from an account the
// approver did not agree to, and the total would still look right.
func TestAnOperationWithItsOwnSourceIsRefused(t *testing.T) {
	c := goldenClient(t)
	a := keypair.MustRandom().Address()
	elsewhere := keypair.MustRandom().Address()

	sneaky := pay(a, "250")
	sneaky.SourceAccount = elsewhere

	_, err := c.DescribeBatch(batchTx(t, pay(a, "250"), sneaky))
	if !errors.Is(err, ErrNotABatch) {
		t.Fatalf("error = %v, want ErrNotABatch", err)
	}
}

func TestAnEmptyBatchIsRefused(t *testing.T) {
	if _, err := AsBatch(&TxDescription{}); !errors.Is(err, ErrNotABatch) {
		t.Fatalf("error = %v, want ErrNotABatch", err)
	}
}

// TestABatchOfOneIsStillABatch: the rules do not soften at one row, because a
// batch of one is what a two-row batch becomes when somebody deletes a line.
func TestABatchOfOneIsStillABatch(t *testing.T) {
	c := goldenClient(t)
	got, err := c.DescribeBatch(batchTx(t, pay(keypair.MustRandom().Address(), "250")))
	if err != nil {
		t.Fatalf("describe batch: %v", err)
	}
	if got.Total != money.MustParse("250") {
		t.Fatalf("total = %s", got.Total)
	}
}

// TestTheTotalTextIsExact: the page compares this string, so it must be the
// exact decimal rather than anything rounded.
func TestTheTotalTextIsExact(t *testing.T) {
	c := goldenClient(t)
	a := keypair.MustRandom().Address()

	got, err := c.DescribeBatch(batchTx(t, pay(a, "0.0000001"), pay(a, "0.0000002")))
	if err != nil {
		t.Fatalf("describe batch: %v", err)
	}
	if got.TotalText() != "0.0000003" {
		t.Fatalf("total text = %q", got.TotalText())
	}
}

// goldenBatchTotal is the total both implementations must produce for the
// batch_payroll corpus case.
//
// Written out here and again in web/static/describe.test.js. Two hardcoded
// strings rather than one shared constant on purpose: the point is that two
// separately-written implementations agree, and a shared constant would let
// them agree by construction.
const goldenBatchTotal = "1500000000.0000003"

func TestTheGoldenBatchTotalsAsBothSidesMustAgree(t *testing.T) {
	c := goldenClient(t)
	tx := goldenCases(t)["batch_payroll"]
	if tx == nil {
		t.Fatal("the corpus has no batch case")
	}

	got, err := c.DescribeBatch(tx)
	if err != nil {
		t.Fatalf("describe batch: %v", err)
	}
	if got.TotalText() != goldenBatchTotal {
		t.Fatalf("total = %s, want %s", got.TotalText(), goldenBatchTotal)
	}
	if len(got.Payments) != 3 {
		t.Fatalf("%d payments", len(got.Payments))
	}
}
