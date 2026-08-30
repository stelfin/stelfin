package settlement

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

func currentSet(master uint32, low, medium, high uint32, signers map[string]uint32) SignerSet {
	if signers == nil {
		signers = map[string]uint32{}
	}
	return SignerSet{MasterWeight: master, Low: low, Medium: medium, High: high, Signers: signers}
}

func TestMultisigBuildsSignersThenThresholds(t *testing.T) {
	one, two := uint32(1), uint32(2)
	ops, err := Multisig(MultisigRequest{
		Account: goldenSource,
		Current: currentSet(1, 1, 1, 1, nil),
		Changes: []SignerChange{
			{Address: goldenSigner, Weight: 1},
			{Address: goldenDest, Weight: 1},
		},
		MasterWeight: &one, Low: &two, Medium: &two, High: &two,
	})
	if err != nil {
		t.Fatalf("Multisig: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("got %d operations, want two signer changes then one threshold change", len(ops))
	}

	// The order is the safety property: lowering the master weight before the
	// new signers exist leaves the account unable to authorise the rest of its
	// own transaction.
	for i := 0; i < 2; i++ {
		op, ok := ops[i].(*txnbuild.SetOptions)
		if !ok || op.Signer == nil {
			t.Fatalf("operation %d is not a signer change: %#v", i, ops[i])
		}
		if op.MasterWeight != nil || op.HighThreshold != nil {
			t.Errorf("operation %d changes a threshold before the signers exist", i)
		}
	}
	last, ok := ops[2].(*txnbuild.SetOptions)
	if !ok || last.Signer != nil {
		t.Fatalf("the last operation is not the threshold change: %#v", ops[2])
	}
	if last.MasterWeight == nil || *last.MasterWeight != 1 || last.HighThreshold == nil {
		t.Errorf("thresholds were not applied: %#v", last)
	}
}

// TestMultisigRefusesLockout is the test this file exists for.
//
// Every other mistake in this system is a payment to the wrong place: bad, and
// somebody still holds the account. Lower the thresholds past what the
// remaining signers can reach and there is no transaction anyone can ever
// submit to fix it, because fixing it needs a signature that can no longer be
// produced.
func TestMultisigRefusesLockout(t *testing.T) {
	zero, three, five := uint32(0), uint32(3), uint32(5)

	cases := map[string]MultisigRequest{
		"master to zero with no other signers": {
			Account:      goldenSource,
			Current:      currentSet(1, 1, 1, 1, nil),
			MasterWeight: &zero,
		},
		"threshold above the weight that would remain": {
			Account: goldenSource,
			Current: currentSet(1, 1, 1, 1, map[string]uint32{goldenSigner: 1}),
			High:    &five,
		},
		"removing the signer the threshold needs": {
			Account: goldenSource,
			Current: currentSet(1, 1, 1, 2, map[string]uint32{goldenSigner: 1}),
			Changes: []SignerChange{{Address: goldenSigner, Weight: 0}},
		},
		"master to zero while raising the bar": {
			Account:      goldenSource,
			Current:      currentSet(1, 1, 1, 1, map[string]uint32{goldenSigner: 2}),
			MasterWeight: &zero,
			High:         &three,
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Multisig(req); !errors.Is(err, ErrLockout) {
				t.Fatalf("error = %v, want ErrLockout — this change would strand the account", err)
			}
		})
	}
}

// TestMultisigRefusesInvertedThresholds: a low above a medium means a payment
// needs more signatures than a signer change, which nobody intends and is a
// reliable sign the arguments are in the wrong order.
func TestMultisigRefusesInvertedThresholds(t *testing.T) {
	low, medium := uint32(3), uint32(1)
	_, err := Multisig(MultisigRequest{
		Account: goldenSource,
		Current: currentSet(5, 1, 1, 1, nil),
		Low:     &low, Medium: &medium,
	})
	if !errors.Is(err, ErrLockout) {
		t.Fatalf("error = %v, want ErrLockout", err)
	}
}

// TestMultisigNeedsTheCurrentSigners: the check is arithmetic over the result,
// so it is only as good as its starting point. A cached signer set is exactly
// what someone being removed would exploit.
func TestMultisigNeedsTheCurrentSigners(t *testing.T) {
	two := uint32(2)
	_, err := Multisig(MultisigRequest{Account: goldenSource, High: &two})
	if err == nil {
		t.Fatal("a signer change was built with no knowledge of the current signers")
	}
	if errors.Is(err, ErrLockout) {
		t.Fatal("refused as a lockout; it should say the current set is missing")
	}
}

func TestMultisigAllowsASafeQuorum(t *testing.T) {
	one, two := uint32(1), uint32(2)
	if _, err := Multisig(MultisigRequest{
		Account: goldenSource,
		Current: currentSet(1, 1, 1, 1, nil),
		Changes: []SignerChange{
			{Address: goldenSigner, Weight: 1},
			{Address: goldenDest, Weight: 1},
		},
		MasterWeight: &one, Low: &two, Medium: &two, High: &two,
	}); err != nil {
		t.Fatalf("a 2-of-3 quorum was refused: %v", err)
	}
}

func TestMultisigRejectsBadInput(t *testing.T) {
	current := currentSet(1, 1, 1, 1, nil)
	for name, req := range map[string]MultisigRequest{
		"bad account": {Account: "not-an-account", Current: current},
		"bad signer": {
			Account: goldenSource, Current: current,
			Changes: []SignerChange{{Address: "nope", Weight: 1}},
		},
		"own key as a signer": {
			Account: goldenSource, Current: current,
			Changes: []SignerChange{{Address: goldenSource, Weight: 1}},
		},
		"weight above the maximum": {
			Account: goldenSource, Current: current,
			Changes: []SignerChange{{Address: goldenSigner, Weight: 256}},
		},
		"changes nothing": {Account: goldenSource, Current: current},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Multisig(req); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPayroll(t *testing.T) {
	lines := []PayrollLine{
		{To: goldenDest, Asset: usdc(), Amount: money.MustParse("10")},
		{To: goldenSigner, Asset: usdc(), Amount: money.MustParse("20.25")},
	}
	ops, err := Payroll(goldenSource, lines)
	if err != nil {
		t.Fatalf("Payroll: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("got %d operations, want 2", len(ops))
	}

	// Every operation names its source. Without it, a batch wrapped in a fee
	// bump would pay from whichever account covered the fee.
	for i, op := range ops {
		payment, ok := op.(*txnbuild.Payment)
		if !ok {
			t.Fatalf("operation %d is not a payment", i)
		}
		if payment.SourceAccount != goldenSource {
			t.Errorf("operation %d has source %q, want the paying account", i, payment.SourceAccount)
		}
	}
}

func TestPayrollRejectsBadLines(t *testing.T) {
	for name, lines := range map[string][]PayrollLine{
		"empty":           {},
		"bad address":     {{To: "nope", Asset: usdc(), Amount: money.MustParse("1")}},
		"zero amount":     {{To: goldenDest, Asset: usdc(), Amount: 0}},
		"negative amount": {{To: goldenDest, Asset: usdc(), Amount: money.MustParse("-1")}},
		"no asset":        {{To: goldenDest, Amount: money.MustParse("1")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Payroll(goldenSource, lines); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestSplitPayrollIsNotAtomic records the property in the place someone will
// look for it: chunks land independently, so a resume has to be per chunk.
func TestSplitPayrollIsNotAtomic(t *testing.T) {
	lines := make([]PayrollLine, 200)
	for i := range lines {
		lines[i] = PayrollLine{To: goldenDest, Asset: usdc(), Amount: money.MustParse("1")}
	}

	chunks := SplitPayroll(lines, 0)
	if len(chunks) != 3 {
		t.Fatalf("split 200 lines into %d chunks, want 3 at %d per transaction",
			len(chunks), PayrollChunkSize)
	}
	var total int
	for _, c := range chunks {
		if len(c) > PayrollChunkSize {
			t.Errorf("a chunk holds %d lines, more than a transaction takes", len(c))
		}
		total += len(c)
	}
	if total != len(lines) {
		t.Errorf("split lost lines: %d of %d", total, len(lines))
	}

	// And each chunk builds on its own, which is what makes a resume possible.
	for i, c := range chunks {
		if _, err := Payroll(goldenSource, c); err != nil {
			t.Errorf("chunk %d does not build: %v", i, err)
		}
	}
}

func TestTotalPayrollIsPerAsset(t *testing.T) {
	native := txnbuild.NativeAsset{}
	totals, err := TotalPayroll([]PayrollLine{
		{To: goldenDest, Asset: usdc(), Amount: money.MustParse("10")},
		{To: goldenDest, Asset: usdc(), Amount: money.MustParse("5")},
		{To: goldenDest, Asset: native, Amount: money.MustParse("2")},
	})
	if err != nil {
		t.Fatalf("TotalPayroll: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals = %v, want one per asset", totals)
	}
	if got := totals["USDC:"+goldenIssuer]; got != money.MustParse("15") {
		t.Errorf("USDC total = %s, want 15", got)
	}
	if got := totals["native"]; got != money.MustParse("2") {
		t.Errorf("XLM total = %s, want 2", got)
	}
}

func TestTrust(t *testing.T) {
	ops, err := Trust(TrustRequest{Account: goldenSource, Asset: usdc()})
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}
	op, ok := ops[0].(*txnbuild.ChangeTrust)
	if !ok {
		t.Fatalf("not a change trust: %#v", ops[0])
	}
	if op.Limit != txnbuild.MaxTrustlineLimit {
		t.Errorf("limit = %q, want the maximum by default", op.Limit)
	}
	if op.SourceAccount != goldenSource {
		t.Errorf("source = %q", op.SourceAccount)
	}
}

func TestUntrustSetsAZeroLimit(t *testing.T) {
	ops, err := Untrust(goldenSource, usdc())
	if err != nil {
		t.Fatalf("Untrust: %v", err)
	}
	if op := ops[0].(*txnbuild.ChangeTrust); op.Limit != "0.0000000" {
		t.Errorf("limit = %q, want zero", op.Limit)
	}
}

// TestTrustRefusesAnIssuersOwnAsset: the network rejects it, and saying so here
// is more useful than discovering it on chain after an approval cycle.
func TestTrustRefusesAnIssuersOwnAsset(t *testing.T) {
	if _, err := Trust(TrustRequest{
		Account: goldenIssuer,
		Asset:   txnbuild.CreditAsset{Code: "USDC", Issuer: goldenIssuer},
	}); err == nil {
		t.Fatal("an issuer was given a trustline to its own asset")
	}
}

func TestOffer(t *testing.T) {
	ops, err := Offer(OfferRequest{
		Account: goldenSource,
		Selling: usdc(), Buying: txnbuild.NativeAsset{},
		Amount: money.MustParse("100"), Price: Price{N: 7, D: 3},
	})
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	op, ok := ops[0].(*txnbuild.ManageSellOffer)
	if !ok {
		t.Fatalf("not a sell offer: %#v", ops[0])
	}
	// The rational survives intact. A decimal round trip is what this avoids.
	if op.Price.N != 7 || op.Price.D != 3 {
		t.Errorf("price = %d/%d, want 7/3", op.Price.N, op.Price.D)
	}
}

func TestOfferRejectsBadInput(t *testing.T) {
	for name, req := range map[string]OfferRequest{
		"same asset both ways": {
			Account: goldenSource, Selling: usdc(), Buying: usdc(),
			Amount: money.MustParse("1"), Price: Price{N: 1, D: 1},
		},
		"cancel with no offer id": {
			Account: goldenSource, Selling: usdc(), Buying: txnbuild.NativeAsset{},
			Amount: 0, Price: Price{N: 1, D: 1},
		},
		"zero price": {
			Account: goldenSource, Selling: usdc(), Buying: txnbuild.NativeAsset{},
			Amount: money.MustParse("1"), Price: Price{N: 0, D: 1},
		},
		"negative amount": {
			Account: goldenSource, Selling: usdc(), Buying: txnbuild.NativeAsset{},
			Amount: money.MustParse("-1"), Price: Price{N: 1, D: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Offer(req); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCancelAnOffer(t *testing.T) {
	ops, err := Offer(OfferRequest{
		Account: goldenSource, Selling: usdc(), Buying: txnbuild.NativeAsset{},
		Amount: 0, OfferID: 42,
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	op := ops[0].(*txnbuild.ManageSellOffer)
	if op.OfferID != 42 || op.Amount != "0.0000000" {
		t.Errorf("cancellation = %#v", op)
	}
}

// TestSwapNeedsASlippageBound is the DEX equivalent of the lockout check.
//
// What a signature commits to is not a quote — the book moves between building
// and inclusion. A default bound would be a number this package invented on
// someone else's behalf, and that number is the whole risk.
func TestSwapNeedsASlippageBound(t *testing.T) {
	req := SwapRequest{
		From: goldenSource, To: goldenDest,
		SendAsset: usdc(), DestAsset: txnbuild.NativeAsset{},
		SendAmount: money.MustParse("100"),
	}
	if _, err := Swap(req); !errors.Is(err, ErrNoSlippageBound) {
		t.Fatalf("error = %v, want ErrNoSlippageBound", err)
	}

	req.MinimumReceived = money.MustParse("230")
	ops, err := Swap(req)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	op := ops[0].(*txnbuild.PathPaymentStrictSend)
	if op.DestMin != "230.0000000" {
		t.Errorf("DestMin = %q, want the floor the signature commits to", op.DestMin)
	}
}

func TestSlippageBound(t *testing.T) {
	quote := money.MustParse("100")

	// One percent.
	got, err := SlippageBound(quote, 100)
	if err != nil {
		t.Fatalf("SlippageBound: %v", err)
	}
	if want := money.MustParse("99"); got != want {
		t.Errorf("bound = %s, want %s", got, want)
	}

	// Zero tolerance is the quote exactly.
	got, err = SlippageBound(quote, 0)
	if err != nil {
		t.Fatalf("SlippageBound: %v", err)
	}
	if got != quote {
		t.Errorf("bound = %s, want the quote %s", got, quote)
	}

	for name, bps := range map[string]int64{
		"negative":      -1,
		"the whole lot": 10_000,
		"more than all": 20_000,
	} {
		if _, err := SlippageBound(quote, bps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := SlippageBound(0, 100); err == nil {
		t.Error("a bound was computed from a zero quote")
	}
}

// TestSlippageBoundTruncatesDownward: rounding up would produce a floor the
// caller never agreed to.
func TestSlippageBoundTruncatesDownward(t *testing.T) {
	// 1 stroop with any tolerance rounds to zero, which is not a usable bound
	// and is refused rather than silently accepting anything.
	if _, err := SlippageBound(money.Stroops(1), 5_000); err == nil {
		t.Fatal("a bound of zero was accepted")
	}

	got, err := SlippageBound(money.Stroops(3), 3_334)
	if err != nil {
		t.Fatalf("SlippageBound: %v", err)
	}
	// 3 * 6666 / 10000 = 1.9998 → 1, not 2.
	if got != money.Stroops(1) {
		t.Errorf("bound = %d stroops, want 1 — it must never round up", got)
	}
}

func TestPriceComparesExactly(t *testing.T) {
	third := Price{N: 1, D: 3}
	twoSixths := Price{N: 2, D: 6}
	half := Price{N: 1, D: 2}

	if third.Cmp(twoSixths) != 0 {
		t.Error("1/3 and 2/6 are the same price")
	}
	if third.Cmp(half) >= 0 {
		t.Error("1/3 is less than 1/2")
	}
	if half.Cmp(third) <= 0 {
		t.Error("1/2 is greater than 1/3")
	}
	if got := third.String(); got != "1/3" {
		t.Errorf("String() = %q, want the rational", got)
	}
}

func TestNewPriceRejectsNonPositiveTerms(t *testing.T) {
	for _, c := range [][2]int32{{0, 1}, {1, 0}, {-1, 2}, {1, -2}} {
		if _, err := NewPrice(c[0], c[1]); err == nil {
			t.Errorf("NewPrice(%d, %d) was accepted", c[0], c[1])
		}
	}
}

func TestBuildRefusesTooManyOperations(t *testing.T) {
	c := goldenClient(t)
	account := &txnbuild.SimpleAccount{AccountID: goldenSource, Sequence: 1}

	ops := make([]txnbuild.Operation, MaxOperations+1)
	for i := range ops {
		ops[i] = &txnbuild.Payment{
			Destination: goldenDest, Amount: "1", Asset: usdc(),
		}
	}
	if _, err := c.buildOn(account, BuildRequest{Source: goldenSource, Operations: ops}); err == nil {
		t.Fatal("a transaction above the protocol limit was built")
	}
	if _, err := c.buildOn(account, BuildRequest{Source: goldenSource}); err == nil {
		t.Fatal("a transaction with no operations was built")
	}
}

func TestBuildComposesOperations(t *testing.T) {
	c := goldenClient(t)
	account := &txnbuild.SimpleAccount{AccountID: goldenSource, Sequence: 1}

	trust, err := Trust(TrustRequest{Account: goldenSource, Asset: usdc()})
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}
	pay, err := Payroll(goldenSource, []PayrollLine{
		{To: goldenDest, Asset: usdc(), Amount: money.MustParse("1")},
	})
	if err != nil {
		t.Fatalf("Payroll: %v", err)
	}

	// The point of returning operations rather than transactions: a trustline
	// and the payment that needs it go in one envelope that lands or does not.
	tx, err := c.buildOn(account, BuildRequest{
		Source:     goldenSource,
		Operations: append(trust, pay...),
	})
	if err != nil {
		t.Fatalf("buildOn: %v", err)
	}
	if len(tx.Operations()) != 2 {
		t.Fatalf("built %d operations, want 2", len(tx.Operations()))
	}

	// And the result describes cleanly, which is what makes it signable.
	if _, err := c.DescribeTx(tx); err != nil {
		t.Fatalf("the composed transaction cannot be described: %v", err)
	}
}
