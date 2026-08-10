package settlement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/support/render/problem"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// fakeHorizon stands in for Horizon so submission semantics can be tested
// without a network. It records whether the ledger was consulted, which is the
// point of several tests below.
type fakeHorizon struct {
	submitResp horizon.Transaction
	submitErr  error

	detailResp horizon.Transaction
	detailErr  error

	detailCalls int
}

func (f *fakeHorizon) AccountDetail(horizonclient.AccountRequest) (horizon.Account, error) {
	return horizon.Account{}, errors.New("not used in this test")
}

func (f *fakeHorizon) SubmitTransactionWithOptions(
	*txnbuild.Transaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return f.submitResp, f.submitErr
}

func (f *fakeHorizon) SubmitFeeBumpTransactionWithOptions(
	*txnbuild.FeeBumpTransaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return f.submitResp, f.submitErr
}

func (f *fakeHorizon) TransactionDetail(string) (horizon.Transaction, error) {
	f.detailCalls++
	return f.detailResp, f.detailErr
}

func testClient(h horizonAPI) *Client {
	return &Client{horizon: h, network: network.TestNetworkPassphrase, baseFee: DefaultBaseFee}
}

func notFoundErr() error {
	return &horizonclient.Error{
		Problem: problem.P{Type: "https://stellar.org/horizon-errors/not_found"},
	}
}

// rejectionErr is what Horizon returns when it has actually evaluated the
// transaction and refused it. The presence of result_codes is what makes the
// outcome definite rather than ambiguous.
func rejectionErr() error {
	return &horizonclient.Error{
		Problem: problem.P{
			Type: "https://stellar.org/horizon-errors/transaction_failed",
			Extras: map[string]interface{}{
				"result_codes": map[string]interface{}{
					"transaction": "tx_bad_seq",
				},
			},
		},
	}
}

func TestBuildProvisionShape(t *testing.T) {
	treasury := keypair.MustRandom().Address()
	user := keypair.MustRandom().Address()
	issuer := keypair.MustRandom().Address()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer}
	line, err := usdc.ToChangeTrustAsset()
	if err != nil {
		t.Fatalf("build trustline asset: %v", err)
	}

	c := testClient(&fakeHorizon{})
	tx, err := c.buildProvisionOn(
		&txnbuild.SimpleAccount{AccountID: treasury, Sequence: 42},
		treasury,
		ProvisionRequest{UserAddress: user, Trustlines: []txnbuild.ChangeTrustAsset{line}},
	)
	if err != nil {
		t.Fatalf("buildProvisionOn: %v", err)
	}

	ops := tx.Operations()
	if len(ops) != 4 {
		t.Fatalf("got %d operations, want 4 (begin, create, trust, end)", len(ops))
	}

	begin, ok := ops[0].(*txnbuild.BeginSponsoringFutureReserves)
	if !ok {
		t.Fatalf("operation 0 is %T, want BeginSponsoringFutureReserves", ops[0])
	}
	if begin.SponsoredID != user {
		t.Errorf("sponsored id = %s, want the user %s", begin.SponsoredID, user)
	}
	if begin.SourceAccount != treasury {
		t.Errorf("begin source = %s, want the treasury %s", begin.SourceAccount, treasury)
	}

	create, ok := ops[1].(*txnbuild.CreateAccount)
	if !ok {
		t.Fatalf("operation 1 is %T, want CreateAccount", ops[1])
	}
	// The whole point of sponsorship: the user's account holds no XLM.
	if create.Amount != "0" {
		t.Errorf("starting balance = %q, want \"0\"; the sponsor covers the reserve", create.Amount)
	}
	if create.Destination != user {
		t.Errorf("create destination = %s, want %s", create.Destination, user)
	}

	trust, ok := ops[2].(*txnbuild.ChangeTrust)
	if !ok {
		t.Fatalf("operation 2 is %T, want ChangeTrust", ops[2])
	}
	// Sourced from the user, which is why the user must sign too.
	if trust.SourceAccount != user {
		t.Errorf("trustline source = %s, want the user %s", trust.SourceAccount, user)
	}

	end, ok := ops[3].(*txnbuild.EndSponsoringFutureReserves)
	if !ok {
		t.Fatalf("operation 3 is %T, want EndSponsoringFutureReserves", ops[3])
	}
	if end.SourceAccount != user {
		t.Errorf("end source = %s, want the user %s", end.SourceAccount, user)
	}

	// Without timebounds the transaction never expires, so a submission that
	// looks failed could still land much later.
	if tx.Timebounds().MaxTime == 0 {
		t.Error("transaction has no maximum time bound")
	}
}

// TestBuildProvisionRequiresBothSignatures pins the property that matters most
// about this transaction: it cannot be executed by the treasury alone.
// Sponsorship is something the user accepts, not something done to them.
func TestBuildProvisionRequiresBothSignatures(t *testing.T) {
	treasuryKP := keypair.MustRandom()
	userKP := keypair.MustRandom()
	issuer := keypair.MustRandom().Address()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer}
	line, err := usdc.ToChangeTrustAsset()
	if err != nil {
		t.Fatalf("build trustline asset: %v", err)
	}

	c := testClient(&fakeHorizon{})
	tx, err := c.buildProvisionOn(
		&txnbuild.SimpleAccount{AccountID: treasuryKP.Address(), Sequence: 1},
		treasuryKP.Address(),
		ProvisionRequest{UserAddress: userKP.Address(), Trustlines: []txnbuild.ChangeTrustAsset{line}},
	)
	if err != nil {
		t.Fatalf("buildProvisionOn: %v", err)
	}

	userSourced, treasurySourced := 0, 0
	for _, op := range tx.Operations() {
		switch op.GetSourceAccount() {
		case userKP.Address():
			userSourced++
		case treasuryKP.Address():
			treasurySourced++
		}
	}
	if userSourced == 0 {
		t.Error("no operation is sourced from the user; the user would not need to sign")
	}
	if treasurySourced == 0 {
		t.Error("no operation is sourced from the treasury")
	}

	// Both signatures must actually apply to the envelope.
	signed, err := tx.Sign(network.TestNetworkPassphrase, treasuryKP, userKP)
	if err != nil {
		t.Fatalf("sign with both keys: %v", err)
	}
	if got := len(signed.Signatures()); got != 2 {
		t.Errorf("signature count = %d, want 2", got)
	}
}

func TestBuildProvisionRejectsMissingTrustline(t *testing.T) {
	c := testClient(&fakeHorizon{})
	_, err := c.BuildProvision(context.Background(), keypair.MustRandom().Address(),
		ProvisionRequest{UserAddress: keypair.MustRandom().Address()})
	if err == nil {
		t.Fatal("expected an error: an account with no trustline cannot receive anything")
	}
}

func signedTx(t *testing.T) *txnbuild.Transaction {
	t.Helper()
	kp := keypair.MustRandom()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: kp.Address(), Sequence: 1},
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.BumpSequence{BumpTo: 100},
		},
		BaseFee:       DefaultBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		t.Fatalf("build transaction: %v", err)
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	return signed
}

func TestSubmitSuccess(t *testing.T) {
	h := &fakeHorizon{submitResp: horizon.Transaction{
		Hash: "abc", Ledger: 7, LedgerCloseTime: time.Unix(1700000000, 0),
	}}
	res, err := testClient(h).Submit(context.Background(), signedTx(t))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Ledger != 7 || res.AlreadyKnown {
		t.Errorf("result = %+v, want ledger 7 and AlreadyKnown false", res)
	}
	if h.detailCalls != 0 {
		t.Errorf("consulted the ledger %d times on a clean success; want 0", h.detailCalls)
	}
}

// TestSubmitAmbiguousButLanded is the case that makes double-spends: the
// request failed, but the transaction is on the ledger. Reporting an error here
// would invite a retry that pays a second time.
func TestSubmitAmbiguousButLanded(t *testing.T) {
	h := &fakeHorizon{
		submitErr: errors.New("connection reset by peer"),
		detailResp: horizon.Transaction{
			Hash: "abc", Ledger: 9, LedgerCloseTime: time.Unix(1700000000, 0),
		},
	}
	res, err := testClient(h).Submit(context.Background(), signedTx(t))
	if err != nil {
		t.Fatalf("Submit after an ambiguous failure: %v", err)
	}
	if !res.AlreadyKnown {
		t.Error("AlreadyKnown = false, want true: the transaction was found on the ledger")
	}
	if res.Ledger != 9 {
		t.Errorf("ledger = %d, want 9", res.Ledger)
	}
	if h.detailCalls != 1 {
		t.Errorf("ledger consulted %d times, want exactly 1", h.detailCalls)
	}
}

func TestSubmitAmbiguousAndAbsent(t *testing.T) {
	h := &fakeHorizon{
		submitErr: errors.New("i/o timeout"),
		detailErr: notFoundErr(),
	}
	_, err := testClient(h).Submit(context.Background(), signedTx(t))
	if err == nil {
		t.Fatal("expected an error when submission failed and nothing is on the ledger")
	}
	if !strings.Contains(err.Error(), "not on the ledger") {
		t.Errorf("error = %v, want it to state the transaction is absent", err)
	}
}

// TestSubmitUnknownOutcome: submission failed and the ledger could not be
// consulted either. The only honest answer is "unknown" — claiming failure
// would be a lie the caller might act on by retrying.
func TestSubmitUnknownOutcome(t *testing.T) {
	h := &fakeHorizon{
		submitErr: errors.New("i/o timeout"),
		detailErr: errors.New("horizon unavailable"),
	}
	_, err := testClient(h).Submit(context.Background(), signedTx(t))
	if err == nil {
		t.Fatal("expected an error when neither outcome could be established")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %v, want it to report the outcome as unknown", err)
	}
}

// TestSubmitDefiniteRejectionSkipsLookup: when Horizon returns result codes it
// has evaluated the transaction and refused it. That is conclusive, so there is
// nothing to reconcile and no reason to spend a round trip.
func TestSubmitDefiniteRejectionSkipsLookup(t *testing.T) {
	h := &fakeHorizon{submitErr: rejectionErr()}
	_, err := testClient(h).Submit(context.Background(), signedTx(t))
	if err == nil {
		t.Fatal("expected an error for a rejected transaction")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want it to report a rejection", err)
	}
	if h.detailCalls != 0 {
		t.Errorf("consulted the ledger %d times after a definite rejection; want 0", h.detailCalls)
	}
}

func TestFeeBumpNamesTreasuryAsFeeAccount(t *testing.T) {
	treasury := keypair.MustRandom()
	inner := signedTx(t)

	bump, err := testClient(&fakeHorizon{}).FeeBump(inner, treasury.Address())
	if err != nil {
		t.Fatalf("FeeBump: %v", err)
	}
	if got := bump.FeeAccount(); got != treasury.Address() {
		t.Errorf("fee account = %s, want the treasury %s", got, treasury.Address())
	}
	// The user's signature must survive untouched: it commits to exactly what
	// they approved, and the bump wraps rather than rewrites it.
	if got := len(bump.InnerTransaction().Signatures()); got != 1 {
		t.Errorf("inner signature count = %d, want 1", got)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{NetworkPassphrase: network.TestNetworkPassphrase}); err == nil {
		t.Error("expected an error when the horizon url is missing")
	}
	if _, err := New(Config{HorizonURL: "https://horizon-testnet.stellar.org"}); err == nil {
		t.Error("expected an error when the network passphrase is missing")
	}
	c, err := New(Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.baseFee != DefaultBaseFee {
		t.Errorf("base fee = %d, want the default %d", c.baseFee, DefaultBaseFee)
	}
}
