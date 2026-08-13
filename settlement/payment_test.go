package settlement

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

func usdcAsset(issuer string) txnbuild.Asset {
	return txnbuild.CreditAsset{Code: "USDC", Issuer: issuer}
}

func buildTestPayment(t *testing.T, amount money.Stroops) (*Client, *txnbuild.Transaction, PaymentRequest) {
	t.Helper()
	from, to, issuer := keypair.MustRandom().Address(), keypair.MustRandom().Address(), keypair.MustRandom().Address()
	req := PaymentRequest{From: from, To: to, Asset: usdcAsset(issuer), Amount: amount}

	c := testClient(&fakeHorizon{})
	tx, err := c.buildPaymentOn(&txnbuild.SimpleAccount{AccountID: from, Sequence: 1}, req)
	if err != nil {
		t.Fatalf("buildPaymentOn: %v", err)
	}
	return c, tx, req
}

// TestDescribeMatchesWhatWasBuilt is the confirmation invariant in test form:
// what a user is shown is read back out of the transaction they will sign, and
// it agrees with what was asked for.
func TestDescribeMatchesWhatWasBuilt(t *testing.T) {
	for _, amount := range []money.Stroops{
		money.MustParse("0.0000001"),
		money.MustParse("1"),
		money.MustParse("5000.50"),
		money.MustParse("922337203685.4775807"),
	} {
		c, tx, req := buildTestPayment(t, amount)

		desc, err := c.Describe(tx)
		if err != nil {
			t.Fatalf("Describe(%s): %v", amount, err)
		}
		if desc.Amount != req.Amount {
			t.Errorf("described amount = %s, want %s", desc.Amount, req.Amount)
		}
		if desc.From != req.From {
			t.Errorf("described source = %s, want %s", desc.From, req.From)
		}
		if desc.To != req.To {
			t.Errorf("described destination = %s, want %s", desc.To, req.To)
		}
		if desc.AssetCode != "USDC" || desc.AssetNative {
			t.Errorf("described asset = %s (native %v), want USDC", desc.AssetCode, desc.AssetNative)
		}
		if desc.Hash == "" {
			t.Error("description carries no hash, so it is not tied to the envelope being signed")
		}
	}
}

// TestDescribeIsTiedToTheEnvelope: the hash in the description must be the hash
// of the transaction that will actually be submitted.
func TestDescribeIsTiedToTheEnvelope(t *testing.T) {
	c, tx, _ := buildTestPayment(t, money.MustParse("10"))

	desc, err := c.Describe(tx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	want, err := tx.HashHex(network.TestNetworkPassphrase)
	if err != nil {
		t.Fatalf("HashHex: %v", err)
	}
	if desc.Hash != want {
		t.Errorf("described hash = %s, want %s", desc.Hash, want)
	}
}

// TestDescribeRefusesMultipleOperations guards the attack the whole mechanism
// exists for: an envelope that shows the user one payment while a second
// operation rides along inside it.
func TestDescribeRefusesMultipleOperations(t *testing.T) {
	from, to, issuer := keypair.MustRandom().Address(), keypair.MustRandom().Address(), keypair.MustRandom().Address()
	attacker := keypair.MustRandom().Address()

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: from, Sequence: 1},
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			// The payment the user would be shown.
			&txnbuild.Payment{Destination: to, Amount: "1.0000000", Asset: usdcAsset(issuer), SourceAccount: from},
			// The one they would not.
			&txnbuild.Payment{Destination: attacker, Amount: "999.0000000", Asset: usdcAsset(issuer), SourceAccount: from},
		},
		BaseFee:       DefaultBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	c := testClient(&fakeHorizon{})
	if _, err := c.Describe(tx); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("Describe error = %v, want ErrIndescribable: a second operation must not ride along", err)
	}
}

func TestDescribeRefusesNonPayment(t *testing.T) {
	from := keypair.MustRandom().Address()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: from, Sequence: 1},
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 100}},
		BaseFee:              DefaultBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	c := testClient(&fakeHorizon{})
	if _, err := c.Describe(tx); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("Describe error = %v, want ErrIndescribable", err)
	}
}

func TestDescribeNativeAsset(t *testing.T) {
	from, to := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	c := testClient(&fakeHorizon{})
	tx, err := c.buildPaymentOn(&txnbuild.SimpleAccount{AccountID: from, Sequence: 1},
		PaymentRequest{From: from, To: to, Asset: txnbuild.NativeAsset{}, Amount: money.MustParse("2.5")})
	if err != nil {
		t.Fatalf("buildPaymentOn: %v", err)
	}

	desc, err := c.Describe(tx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !desc.AssetNative || desc.AssetCode != "XLM" {
		t.Errorf("described asset = %s (native %v), want native XLM", desc.AssetCode, desc.AssetNative)
	}
	if desc.AssetIssuer != "" {
		t.Errorf("native asset carries issuer %q, want empty", desc.AssetIssuer)
	}
}

func TestBuildPaymentRejectsBadRequests(t *testing.T) {
	c := testClient(&fakeHorizon{})
	addr := keypair.MustRandom().Address()
	other := keypair.MustRandom().Address()
	issuer := keypair.MustRandom().Address()
	acct := &txnbuild.SimpleAccount{AccountID: addr, Sequence: 1}

	// Zero and negative are caught before a transaction is built at all; these
	// go through the exported entry point for the guard, which does not need a
	// network round trip to reject them.
	for name, req := range map[string]PaymentRequest{
		"zero amount":     {From: addr, To: other, Asset: usdcAsset(issuer), Amount: 0},
		"negative amount": {From: addr, To: other, Asset: usdcAsset(issuer), Amount: money.MustParse("-1")},
		"same account":    {From: addr, To: addr, Asset: usdcAsset(issuer), Amount: money.MustParse("1")},
		"missing to":      {From: addr, Asset: usdcAsset(issuer), Amount: money.MustParse("1")},
		"missing from":    {To: other, Asset: usdcAsset(issuer), Amount: money.MustParse("1")},
	} {
		if _, err := c.BuildPayment(t.Context(), req); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// A well-formed request still builds when given an account directly.
	if _, err := c.buildPaymentOn(acct, PaymentRequest{
		From: addr, To: other, Asset: usdcAsset(issuer), Amount: money.MustParse("1"),
	}); err != nil {
		t.Errorf("well-formed request failed to build: %v", err)
	}
}

// TestPaymentAmountSurvivesTheWire: the amount encoded into the transaction
// must round-trip exactly, including values beyond float64's mantissa.
func TestPaymentAmountSurvivesTheWire(t *testing.T) {
	for _, amount := range []money.Stroops{
		1, 1 << 53, 1<<53 + 1, money.MustParse("922337203685.4775807"),
	} {
		c, tx, _ := buildTestPayment(t, amount)
		desc, err := c.Describe(tx)
		if err != nil {
			t.Fatalf("Describe(%d): %v", int64(amount), err)
		}
		if desc.Amount != amount {
			t.Errorf("amount %d became %d through the transaction", int64(amount), int64(desc.Amount))
		}
	}
}
