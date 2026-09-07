package settlement

import (
	"context"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// accountFake answers AccountDetail and nothing else. A separate fake from
// fakeHorizon on purpose: a submission reaching this test would be a bug in the
// test rather than something to tolerate.
type accountFake struct {
	account horizon.Account
	err     error
	calls   int
}

func (a *accountFake) AccountDetail(req horizonclient.AccountRequest) (horizon.Account, error) {
	a.calls++
	out := a.account
	// Echo the account that was asked for when a test has not pinned one. A
	// builder handed an account with no id produces an envelope with no source,
	// which fails for a reason that has nothing to do with what is under test.
	if out.AccountID == "" {
		out.AccountID = req.AccountID
	}
	return out, a.err
}

func (a *accountFake) SubmitTransactionWithOptions(
	*txnbuild.Transaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("this test must not submit")
}

func (a *accountFake) SubmitFeeBumpTransactionWithOptions(
	*txnbuild.FeeBumpTransaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("this test must not submit")
}

func (a *accountFake) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("this test must not look up a transaction")
}

func TestSignerSetOfSeparatesTheMasterKey(t *testing.T) {
	account := keypair.MustRandom().Address()
	other := keypair.MustRandom().Address()

	h := &accountFake{account: horizon.Account{
		AccountID: account,
		Thresholds: horizon.AccountThresholds{
			LowThreshold: 1, MedThreshold: 2, HighThreshold: 3,
		},
		Signers: []horizon.Signer{
			{Key: account, Weight: 1, Type: "ed25519_public_key"},
			{Key: other, Weight: 2, Type: "ed25519_public_key"},
		},
	}}

	got, err := testClient(h).SignerSetOf(context.Background(), account)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}

	// The master key is addressed through MasterWeight rather than through a
	// Signer operation; conflating the two builds an envelope that locks the
	// account out of itself.
	if got.MasterWeight != 1 {
		t.Errorf("MasterWeight = %d, want 1", got.MasterWeight)
	}
	if len(got.Signers) != 1 || got.Signers[other] != 2 {
		t.Errorf("Signers = %v", got.Signers)
	}
	if got.Medium != 2 || got.High != 3 {
		t.Errorf("thresholds = %d/%d/%d", got.Low, got.Medium, got.High)
	}
	if got.TotalWeight() != 3 {
		t.Errorf("TotalWeight = %d, want 3", got.TotalWeight())
	}

	// The SEP-10 summary puts the master key back, under the account's own
	// address — a conversion that dropped it would report a good proof as
	// insufficient.
	summary := got.Summary(account)
	if summary[account] != 1 || summary[other] != 2 || len(summary) != 2 {
		t.Errorf("Summary = %v", summary)
	}
}

// TestSignerSetOfCountsSignersItCannotUse: a hash(x) or pre-authorised
// transaction signer carries real weight and can never sign a challenge.
// Dropping it silently would make a treasury look unprovable for no stated
// reason.
func TestSignerSetOfCountsSignersItCannotUse(t *testing.T) {
	account := keypair.MustRandom().Address()

	h := &accountFake{account: horizon.Account{
		AccountID:  account,
		Thresholds: horizon.AccountThresholds{MedThreshold: 2},
		Signers: []horizon.Signer{
			{Key: account, Weight: 1, Type: "ed25519_public_key"},
			{Key: "XDRPF6NZRR7EEVO7ESIWUDXHAOMM2QSKIQQBJK6I2FB7YKDZES5UCLWD",
				Weight: 5, Type: "sha256_hash"},
		},
	}}

	got, err := testClient(h).SignerSetOf(context.Background(), account)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	if got.SkippedNonKeySigners != 1 {
		t.Errorf("SkippedNonKeySigners = %d, want 1", got.SkippedNonKeySigners)
	}
	if len(got.Signers) != 0 {
		t.Errorf("a non-key signer was kept: %v", got.Signers)
	}
	if got.TotalWeight() != 1 {
		t.Errorf("TotalWeight = %d, want 1; weight it cannot sign with must not count",
			got.TotalWeight())
	}
}

func TestSignerSetOfDropsRemovedSigners(t *testing.T) {
	account := keypair.MustRandom().Address()
	gone := keypair.MustRandom().Address()

	h := &accountFake{account: horizon.Account{
		AccountID: account,
		Signers: []horizon.Signer{
			{Key: account, Weight: 1, Type: "ed25519_public_key"},
			{Key: gone, Weight: 0, Type: "ed25519_public_key"},
		},
	}}

	got, err := testClient(h).SignerSetOf(context.Background(), account)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	if _, still := got.Signers[gone]; still {
		t.Error("a signer at weight zero was kept; the bot would name them as a pending approver")
	}
}

func TestSignerSetOfRefusesSomethingThatIsNotAnAccount(t *testing.T) {
	h := &accountFake{}
	for _, address := range []string{
		"",
		"CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE",
		"not an address",
	} {
		if _, err := testClient(h).SignerSetOf(context.Background(), address); err == nil {
			t.Errorf("accepted %q", address)
		}
	}
	if h.calls != 0 {
		t.Errorf("Horizon was called %d time(s) for an address that cannot be one", h.calls)
	}
}
