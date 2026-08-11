package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/ezedike-evan/stelfin/api/intent"
	"github.com/ezedike-evan/stelfin/internal/money"
	"github.com/ezedike-evan/stelfin/internal/pgtest"
	"github.com/ezedike-evan/stelfin/ledger"
	"github.com/ezedike-evan/stelfin/settlement"
)

// testPGPort must differ from every other package's: `go test ./...` runs
// packages in parallel and two Postgres servers cannot share a port.
const testPGPort = 54332

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	db, err := pgtest.Start(testPGPort, ledger.Migrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testPool = db.Pool

	code := m.Run()

	if err := db.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// fakeHorizon serves the sender's account so a transaction can be built with
// no network.
type fakeHorizon struct{ sequence int64 }

func (f *fakeHorizon) AccountDetail(req horizonclient.AccountRequest) (horizon.Account, error) {
	return horizon.Account{
		AccountID: req.AccountID,
		Sequence:  f.sequence,
	}, nil
}

func (f *fakeHorizon) SubmitTransactionWithOptions(
	*txnbuild.Transaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("not used")
}

func (f *fakeHorizon) SubmitFeeBumpTransactionWithOptions(
	*txnbuild.FeeBumpTransaction, horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("not used")
}

func (f *fakeHorizon) TransactionDetail(string) (horizon.Transaction, error) {
	return horizon.Transaction{}, errors.New("not used")
}

// fixedDecoder returns whatever a test says the model proposed. It stands in
// for a language model precisely because the pipeline must be safe regardless
// of what the model says.
type fixedDecoder struct {
	decoded intent.Decoded
	err     error
}

func (d fixedDecoder) Decode(context.Context, []string) (intent.Decoded, error) {
	return d.decoded, d.err
}

var testIssuer = keypair.MustRandom().Address()

type fixture struct {
	svc      *Service
	owner    string
	from     string
	toAddr   string
	assetXDR txnbuild.Asset
}

func newFixture(t *testing.T, decoded intent.Decoded) *fixture {
	t.Helper()
	ctx := context.Background()
	owner := t.Name()

	store := ledger.New(testPool)
	account, err := store.EnsureAccount(ctx, ledger.AccountUser, owner, "user "+owner)
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}

	from := keypair.MustRandom().Address()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO stellar_accounts (address, ledger_account_id) VALUES ($1, $2)`,
		from, int64(account)); err != nil {
		t.Fatalf("track sender address: %v", err)
	}

	toAddr := keypair.MustRandom().Address()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO beneficiaries (owner_ref, label, address) VALUES ($1, $2, $3)`,
		owner, "Brother", toAddr); err != nil {
		t.Fatalf("save beneficiary: %v", err)
	}

	asset := txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}
	settle, err := settlement.NewWith(&fakeHorizon{sequence: 42}, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}

	svc, err := NewService(testPool, fixedDecoder{decoded: decoded},
		intent.NewResolver(testPool), settle,
		Config{Asset: asset, AssetCode: "USDC"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	return &fixture{svc: svc, owner: owner, from: from, toAddr: toAddr, assetXDR: asset}
}

// sendDecoded is a well-formed decode of "send 5,000 to brother".
func sendDecoded() intent.Decoded {
	return intent.Decoded{
		Action:          intent.Field{Text: "send", Span: intent.Span{Turn: 0, Start: 0, End: 1}},
		Amount:          intent.Field{Text: "5,000", Span: intent.Span{Turn: 0, Start: 1, End: 2}},
		Destination:     intent.Field{Text: "brother", Span: intent.Span{Turn: 0, Start: 3, End: 4}},
		DestinationKind: intent.DestinationBeneficiary,
	}
}

const sendMessage = "send 5,000 to brother"

func TestPrepareSend(t *testing.T) {
	f := newFixture(t, sendDecoded())

	got, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	if want := money.MustParse("5000"); got.Amount != want {
		t.Errorf("amount = %s, want %s", got.Amount, want)
	}
	if got.AmountDisplay != "5,000.00" {
		t.Errorf("display = %q, want %q", got.AmountDisplay, "5,000.00")
	}
	if got.ToAddress != f.toAddr {
		t.Errorf("destination = %s, want %s", got.ToAddress, f.toAddr)
	}
	if got.ToLabel != "Brother" {
		t.Errorf("label = %q, want the saved %q", got.ToLabel, "Brother")
	}
	if got.FromAddress != f.from {
		t.Errorf("source = %s, want %s", got.FromAddress, f.from)
	}
	// The user's own words come back so the screen can show which phrase
	// produced this.
	if got.SaidAmount != "5,000" || got.SaidDestination != "brother" {
		t.Errorf("echoed words = %q / %q, want %q / %q",
			got.SaidAmount, got.SaidDestination, "5,000", "brother")
	}
	if got.XDR == "" || got.Hash == "" {
		t.Error("confirmation carries no transaction")
	}
}

// TestConfirmationMatchesTheSignedEnvelope is the invariant that matters most
// in this package. It decodes the XDR the client will sign, independently of
// the confirmation, and checks they agree. If a future change ever let the
// screen drift from the envelope, this fails.
func TestConfirmationMatchesTheSignedEnvelope(t *testing.T) {
	f := newFixture(t, sendDecoded())

	got, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	parsed, err := txnbuild.TransactionFromXDR(got.XDR)
	if err != nil {
		t.Fatalf("parse XDR: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("XDR is not a plain transaction")
	}

	ops := tx.Operations()
	if len(ops) != 1 {
		t.Fatalf("envelope carries %d operations, want exactly 1", len(ops))
	}
	payment, ok := ops[0].(*txnbuild.Payment)
	if !ok {
		t.Fatalf("envelope operation is %T, want a payment", ops[0])
	}

	amount, err := money.Parse(payment.Amount)
	if err != nil {
		t.Fatalf("parse envelope amount: %v", err)
	}
	if amount != got.Amount {
		t.Errorf("envelope pays %s but the user is shown %s", amount, got.Amount)
	}
	if payment.Destination != got.ToAddress {
		t.Errorf("envelope pays %s but the user is shown %s", payment.Destination, got.ToAddress)
	}

	hash, err := tx.HashHex(network.TestNetworkPassphrase)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash != got.Hash {
		t.Errorf("envelope hash %s does not match the confirmed %s", hash, got.Hash)
	}
}

// TestHallucinatedAmountNeverReachesATransaction: verification runs before
// anything is built, so a bad decode cannot even produce something to sign.
func TestHallucinatedAmountNeverReachesATransaction(t *testing.T) {
	d := sendDecoded()
	d.Amount.Text = "50,000" // the span still says 5,000

	f := newFixture(t, d)
	got, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage})
	if !errors.Is(err, intent.ErrSpanMismatch) {
		t.Fatalf("error = %v, want ErrSpanMismatch", err)
	}
	if got != nil {
		t.Error("a confirmation was produced for a rejected decode")
	}
}

func TestUnknownBeneficiaryIsRefused(t *testing.T) {
	d := sendDecoded()
	d.Destination = intent.Field{Text: "stranger", Span: intent.Span{Turn: 0, Start: 3, End: 4}}

	f := newFixture(t, d)
	// The span says "brother", so this is caught as a mismatch before the
	// lookup even happens.
	if _, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnsavedBeneficiaryIsRefused(t *testing.T) {
	msg := "send 5,000 to landlord"
	d := sendDecoded()
	d.Destination = intent.Field{Text: "landlord", Span: intent.Span{Turn: 0, Start: 3, End: 4}}

	f := newFixture(t, d)
	_, err := f.svc.PrepareSend(context.Background(), f.owner, []string{msg})
	if !errors.Is(err, intent.ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound", err)
	}
}

func TestNonSendIsRefused(t *testing.T) {
	f := newFixture(t, intent.Decoded{
		Action: intent.Field{Text: "balance", Span: intent.Span{Turn: 0, Start: 3, End: 4}},
	})

	_, err := f.svc.PrepareSend(context.Background(), f.owner, []string{"what is my balance"})
	if !errors.Is(err, ErrNotASend) {
		t.Fatalf("error = %v, want ErrNotASend", err)
	}
}

func TestUserWithoutAnAccountIsRefused(t *testing.T) {
	ctx := context.Background()
	owner := t.Name()

	// A beneficiary but no provisioned Stellar account.
	toAddr := keypair.MustRandom().Address()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO beneficiaries (owner_ref, label, address) VALUES ($1, $2, $3)`,
		owner, "Brother", toAddr); err != nil {
		t.Fatalf("save beneficiary: %v", err)
	}

	settle, err := settlement.NewWith(&fakeHorizon{sequence: 1}, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{Asset: txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}, AssetCode: "USDC"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if _, err := svc.PrepareSend(ctx, owner, []string{sendMessage}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("error = %v, want ErrNoAccount", err)
	}
}

func TestDecoderFailureIsReported(t *testing.T) {
	f := newFixture(t, sendDecoded())
	f.svc.decoder = fixedDecoder{err: errors.New("model unavailable")}

	if _, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage}); err == nil {
		t.Fatal("expected the decoder error to surface")
	}
}

func TestEmptyConversationIsRefused(t *testing.T) {
	f := newFixture(t, sendDecoded())
	if _, err := f.svc.PrepareSend(context.Background(), f.owner, nil); err == nil {
		t.Fatal("expected an error for an empty conversation")
	}
}

func TestNewServiceValidatesConfig(t *testing.T) {
	if _, err := NewService(testPool, nil, nil, nil, Config{AssetCode: "USDC"}); err == nil {
		t.Error("expected an error when the asset is missing")
	}
	if _, err := NewService(testPool, nil, nil, nil,
		Config{Asset: txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}}); err == nil {
		t.Error("expected an error when the asset code is missing")
	}
}
