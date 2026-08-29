package settlement

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// The golden corpus.
//
// Each case is written out as XDR plus the canonical description this
// implementation produces. A Node test reads the same files and asserts the
// browser's renderer produces the same canonical bytes from the same XDR.
//
// Without that corpus the two implementations drift within weeks and the
// guarantee evaporates silently — the page would go on comparing its own output
// to the server's, both wrong in the same way, and nobody would learn anything.
//
// Regenerate with: UPDATE_GOLDEN=1 go test ./settlement/

var updateGolden = os.Getenv("UPDATE_GOLDEN") != ""

// Fixed addresses, derived from constant seeds, so a golden file is stable
// across runs and across machines. Real accounts with real checksums, because
// the builder validates them and a fixture that cannot be built proves nothing.
const (
	goldenSource = "GCFIRY65OQE7DFP5KLNS2PF2LVZMUZYJX4OZIEQ36N2IQANUB5XVYOJR"
	goldenDest   = "GCATS5YOVB6ROX2WUNKGNQ2MP3GMXDMKSG2O4N5CLX3A6W4PZGZZI55U"
	goldenIssuer = "GDWUSKGGFDI4FRXK5EBTRECZSVQSSWJHHJOGH6JWG3AUMFFMQ435DIAG"
	goldenSigner = "GDFJHLAXAUMHA4OWPOB4P7YO72AQR2HMIUYFOXLXE2DZGM633K7HZDQP"
)

func goldenClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewWith(&fakeHorizon{}, Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// buildGolden assembles a transaction with fixed values throughout, so the
// canonical output depends only on the operations.
func buildGolden(t *testing.T, memo txnbuild.Memo, ops ...txnbuild.Operation) *txnbuild.Transaction {
	t.Helper()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: goldenSource, Sequence: 41},
		IncrementSequenceNum: true,
		BaseFee:              10_000,
		Memo:                 memo,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimebounds(1_700_000_000, 1_700_000_180),
		},
		Operations: ops,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tx
}

func usdc() txnbuild.CreditAsset {
	return txnbuild.CreditAsset{Code: "USDC", Issuer: goldenIssuer}
}

// goldenCase is one file in the corpus.
type goldenCase struct {
	Name      string `json:"name"`
	XDR       string `json:"xdr"`
	Network   string `json:"network"`
	Canonical string `json:"canonical"`
}

func goldenCases(t *testing.T) map[string]*txnbuild.Transaction {
	t.Helper()
	weight := txnbuild.Threshold(2)
	master := txnbuild.Threshold(0)
	domain := "stelfin.example"

	return map[string]*txnbuild.Transaction{
		"payment": buildGolden(t, nil, &txnbuild.Payment{
			Destination: goldenDest, Amount: "5000", Asset: usdc(),
		}),

		// "5000" and "5000.0000000" are the same amount and must describe
		// identically, or two correct readers disagree about one transaction.
		"payment_normalised_amount": buildGolden(t, nil, &txnbuild.Payment{
			Destination: goldenDest, Amount: "5000.0000000", Asset: usdc(),
		}),

		"payment_native_with_memo": buildGolden(t, txnbuild.MemoText("rent"), &txnbuild.Payment{
			Destination: goldenDest, Amount: "1.5", Asset: txnbuild.NativeAsset{},
		}),

		// A memo carrying the format's own separators. If escaping is wrong,
		// this is where it shows.
		"payment_memo_with_separators": buildGolden(t,
			txnbuild.MemoText("a\tb\nc\\d"), &txnbuild.Payment{
				Destination: goldenDest, Amount: "1", Asset: usdc(),
			}),

		"sponsored_creation": buildGolden(t, nil,
			&txnbuild.BeginSponsoringFutureReserves{SponsoredID: goldenDest},
			&txnbuild.CreateAccount{Destination: goldenDest, Amount: "0"},
			&txnbuild.ChangeTrust{
				SourceAccount: goldenDest,
				Line:          usdc().MustToChangeTrustAsset(),
				Limit:         txnbuild.MaxTrustlineLimit,
			},
			&txnbuild.EndSponsoringFutureReserves{SourceAccount: goldenDest},
		),

		"payroll_batch": buildGolden(t, nil,
			&txnbuild.Payment{Destination: goldenDest, Amount: "10", Asset: usdc()},
			&txnbuild.Payment{Destination: goldenSigner, Amount: "20.25", Asset: usdc()},
			&txnbuild.Payment{Destination: goldenIssuer, Amount: "0.0000001", Asset: usdc()},
		),

		"set_options_multisig": buildGolden(t, nil, &txnbuild.SetOptions{
			Signer:          &txnbuild.Signer{Address: goldenSigner, Weight: 1},
			MasterWeight:    &master,
			LowThreshold:    &weight,
			MediumThreshold: &weight,
			HighThreshold:   &weight,
			HomeDomain:      &domain,
		}),

		"manage_sell_offer": buildGolden(t, nil, &txnbuild.ManageSellOffer{
			Selling: usdc(), Buying: txnbuild.NativeAsset{},
			Amount: "100", Price: xdr.Price{N: 7, D: 3}, OfferID: 0,
		}),

		"path_payment_strict_send": buildGolden(t, nil, &txnbuild.PathPaymentStrictSend{
			SendAsset: usdc(), SendAmount: "100",
			Destination: goldenDest, DestAsset: txnbuild.NativeAsset{}, DestMin: "230.5",
			Path: []txnbuild.Asset{txnbuild.NativeAsset{}},
		}),

		"change_trust_removal": buildGolden(t, nil, &txnbuild.ChangeTrust{
			Line: usdc().MustToChangeTrustAsset(), Limit: "0",
		}),

		"account_merge": buildGolden(t, nil, &txnbuild.AccountMerge{Destination: goldenDest}),

		"manage_data": buildGolden(t, nil, &txnbuild.ManageData{
			Name: "config", Value: []byte{0x00, 0xff, 0x10},
		}),

		"bump_sequence": buildGolden(t, nil, &txnbuild.BumpSequence{BumpTo: 9_000_000}),
	}
}

func goldenDir() string { return filepath.Join("testdata", "describe") }

func TestDescribeMatchesTheGoldenCorpus(t *testing.T) {
	c := goldenClient(t)
	cases := goldenCases(t)

	if updateGolden {
		if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
			t.Fatalf("make golden dir: %v", err)
		}
	}

	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := c.DescribeTx(tx)
			if err != nil {
				t.Fatalf("DescribeTx: %v", err)
			}
			xdrText, err := tx.Base64()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			path := filepath.Join(goldenDir(), name+".json")
			got := goldenCase{
				Name: name, XDR: xdrText,
				Network: network.TestNetworkPassphrase, Canonical: d.Canonical(),
			}

			if updateGolden {
				encoded, err := json.MarshalIndent(got, "", "  ")
				if err != nil {
					t.Fatalf("encode golden: %v", err)
				}
				if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
			}
			var want goldenCase
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode golden: %v", err)
			}

			if got.XDR != want.XDR {
				t.Fatalf("the fixture changed; regenerate with UPDATE_GOLDEN=1")
			}
			if got.Canonical != want.Canonical {
				t.Errorf("canonical description changed\n got:\n%s\nwant:\n%s",
					got.Canonical, want.Canonical)
			}
		})
	}
}

// TestCanonicalIsStable: the same transaction described twice must produce
// identical bytes. A map iteration or a time-dependent value anywhere in the
// renderer shows up here.
func TestCanonicalIsStable(t *testing.T) {
	c := goldenClient(t)
	for name, tx := range goldenCases(t) {
		first, err := c.DescribeTx(tx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i := 0; i < 20; i++ {
			again, err := c.DescribeTx(tx)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if first.Canonical() != again.Canonical() {
				t.Fatalf("%s: canonical form is not stable across runs", name)
			}
		}
	}
}

// TestCanonicalBindsTheHash: a description that matched but named a different
// transaction would be a correct description of the wrong thing.
func TestCanonicalBindsTheHash(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil, &txnbuild.Payment{
		Destination: goldenDest, Amount: "1", Asset: usdc(),
	})
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if !strings.Contains(d.Canonical(), d.Hash) {
		t.Fatal("the canonical form does not name the transaction it describes")
	}
}

// TestDescribeRefusesWhatItCannotShow is the rule the whole layer rests on.
//
// Not "one operation" — that was never the valuable half. It is: refuse to
// produce a signable artifact containing anything the renderer cannot fully
// describe. A renderer that showed what it understood and dropped the rest
// would be a downgrade dressed as a feature.
func TestDescribeRefusesWhatItCannotShow(t *testing.T) {
	c := goldenClient(t)

	// An operation type this build does not render. Inflation is a real
	// operation and deliberately not in the switch.
	tx := buildGolden(t, nil,
		&txnbuild.Payment{Destination: goldenDest, Amount: "1", Asset: usdc()},
		&txnbuild.Inflation{},
	)
	if _, err := c.DescribeTx(tx); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("error = %v, want ErrIndescribable", err)
	}
}

// TestMultipleOperationsAreDescribed is the other half: what used to be refused
// outright is now shown in full.
func TestMultipleOperationsAreDescribed(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil,
		&txnbuild.Payment{Destination: goldenDest, Amount: "1", Asset: usdc()},
		&txnbuild.Payment{Destination: goldenSigner, Amount: "2", Asset: usdc()},
	)

	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if len(d.Operations) != 2 {
		t.Fatalf("described %d operations, want 2", len(d.Operations))
	}
	for i, op := range d.Operations {
		if op.Index != i || op.Summary == "" || len(op.Fields) == 0 {
			t.Errorf("operation %d is not fully described: %+v", i, op)
		}
	}
}

// TestOperationSourceIsEffective: an operation that names its own source is
// spending from a different account than the transaction's, and a reader must
// be able to see that.
func TestOperationSourceIsEffective(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil,
		&txnbuild.Payment{Destination: goldenDest, Amount: "1", Asset: usdc()},
		&txnbuild.Payment{
			SourceAccount: goldenSigner,
			Destination:   goldenDest, Amount: "1", Asset: usdc(),
		},
	)
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if d.Operations[0].Source != goldenSource {
		t.Errorf("operation 0 source = %s, want the transaction's", d.Operations[0].Source)
	}
	if d.Operations[1].Source != goldenSigner {
		t.Errorf("operation 1 source = %s, want its own", d.Operations[1].Source)
	}
}

// TestSinglePaymentBridgesTheOldShape: callers written when a transaction could
// only be one payment get the same answer through the general renderer, so
// there is one implementation of "what does this do" rather than two.
func TestSinglePaymentBridgesTheOldShape(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil, &txnbuild.Payment{
		Destination: goldenDest, Amount: "5000", Asset: usdc(),
	})

	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	p, ok := d.SinglePayment()
	if !ok {
		t.Fatal("a single payment was not recognised as one")
	}
	if p.To != goldenDest || p.Amount.String() != "5000.0000000" || p.AssetCode != "USDC" {
		t.Errorf("payment = %+v", p)
	}
	if p.Hash != d.Hash {
		t.Error("the bridged description names a different transaction")
	}
}

// TestSinglePaymentRefusesMore: a caller asking for "the payment" must not be
// handed the first of several while the rest ride along unmentioned.
func TestSinglePaymentRefusesMore(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil,
		&txnbuild.Payment{Destination: goldenDest, Amount: "1", Asset: usdc()},
		&txnbuild.Payment{Destination: goldenSigner, Amount: "2", Asset: usdc()},
	)
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if _, ok := d.SinglePayment(); ok {
		t.Fatal("a two-payment transaction was bridged as a single payment")
	}
}

// TestPriceIsARational: dividing to render a price would introduce the one
// thing this layer exists to keep out.
func TestPriceIsARational(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil, &txnbuild.ManageSellOffer{
		Selling: usdc(), Buying: txnbuild.NativeAsset{},
		Amount: "1", Price: xdr.Price{N: 1, D: 3},
	})
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	var price string
	for _, f := range d.Operations[0].Fields {
		if f.Label == "price" {
			price = f.Value
		}
	}
	if price != "1/3" {
		t.Errorf("price = %q, want the rational 1/3 rather than a decimal", price)
	}
}

// TestEscapingSurvivesSeparators: a memo can contain anything, including the
// characters the line format is built from.
func TestEscapingSurvivesSeparators(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, txnbuild.MemoText("a\tb\nc\\d"), &txnbuild.Payment{
		Destination: goldenDest, Amount: "1", Asset: usdc(),
	})
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	canonical := d.Canonical()

	// The memo occupies exactly one line, whatever it contained.
	var memoLines int
	for _, line := range strings.Split(canonical, "\n") {
		if strings.HasPrefix(line, "memo\t") {
			memoLines++
		}
	}
	if memoLines != 1 {
		t.Fatalf("the memo produced %d lines; escaping is broken:\n%s", memoLines, canonical)
	}
	if !strings.Contains(canonical, `a\tb\nc\\d`) {
		t.Errorf("the memo was not escaped as expected:\n%s", canonical)
	}
}

// TestFeeBumpDescribesTheInnerTransaction: the signatures commit to the inner
// one; the outer envelope only says who pays.
func TestFeeBumpDescribesTheInnerTransaction(t *testing.T) {
	c := goldenClient(t)
	inner := buildGolden(t, nil, &txnbuild.Payment{
		Destination: goldenDest, Amount: "1", Asset: usdc(),
	})
	signed, err := inner.Sign(network.TestNetworkPassphrase, keypair.MustRandom())
	if err != nil {
		t.Fatalf("sign inner: %v", err)
	}
	bump, err := c.FeeBump(signed, goldenSource)
	if err != nil {
		t.Fatalf("FeeBump: %v", err)
	}

	d, err := c.DescribeFeeBump(bump)
	if err != nil {
		t.Fatalf("DescribeFeeBump: %v", err)
	}
	if d.Kind != "fee_bump" {
		t.Errorf("kind = %q", d.Kind)
	}
	if len(d.Operations) != 1 || d.Operations[0].Type != "payment" {
		t.Errorf("the inner payment is not described: %+v", d.Operations)
	}
	innerHash, _ := signed.HashHex(network.TestNetworkPassphrase)
	if d.Hash == innerHash {
		t.Error("the fee bump is named by the inner hash; they are different envelopes")
	}
}

func TestDescribeRefusesAnEmptyTransaction(t *testing.T) {
	c := goldenClient(t)
	if _, err := c.DescribeTx(nil); !errors.Is(err, ErrIndescribable) {
		t.Fatalf("error = %v, want ErrIndescribable", err)
	}
}

// TestTimeBoundsAreDescribed: an envelope's validity window is part of what a
// signature commits to.
func TestTimeBoundsAreDescribed(t *testing.T) {
	c := goldenClient(t)
	tx := buildGolden(t, nil, &txnbuild.Payment{
		Destination: goldenDest, Amount: "1", Asset: usdc(),
	})
	d, err := c.DescribeTx(tx)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if d.MaxTime != "1700000180" {
		t.Errorf("max_time = %q", d.MaxTime)
	}
	if d.MinTime != "1700000000" {
		t.Errorf("min_time = %q", d.MinTime)
	}
}
