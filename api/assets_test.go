package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

var eurcIssuer = keypair.MustRandom().Address()

func line(code, issuer, balance string) horizon.Balance {
	return horizon.Balance{
		Balance: balance, Limit: "922337203685.4775807",
		Asset: base.Asset{Type: "credit_alphanum4", Code: code, Issuer: issuer},
	}
}

func xlm(balance string) horizon.Balance {
	return horizon.Balance{Balance: balance, Asset: base.Asset{Type: "native"}}
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var r *RefusalError
	if !errors.As(err, &r) {
		t.Fatalf("error = %v, want a refusal with a reason", err)
	}
	return r.Reason
}

// ops returns the operations of a proposal's own envelope.
func ops(t *testing.T, view *ProposalView) []txnbuild.Operation {
	t.Helper()
	tx, err := transactionFrom(view.Proposal.XDR)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	return tx.Operations()
}

func TestTrustProposesOneChangeTrustFromTheTreasury(t *testing.T) {
	f := newProposalFixture(t, "trust")
	ctx := context.Background()

	view, name, err := f.svc.ProposeTrust(ctx, f.scope, f.treasuryRow.ID, "EURC:"+eurcIssuer, f.member)
	if err != nil {
		t.Fatalf("ProposeTrust: %v", err)
	}
	if view.Proposal.Kind != "trustline" || name != "EURC:"+eurcIssuer {
		t.Fatalf("kind %q, name %q", view.Proposal.Kind, name)
	}
	got := ops(t, view)
	change, ok := got[0].(*txnbuild.ChangeTrust)
	if len(got) != 1 || !ok {
		t.Fatalf("operations = %#v, want one ChangeTrust", got)
	}
	if change.Line.GetCode() != "EURC" || change.Line.GetIssuer() != eurcIssuer || change.Limit != txnbuild.MaxTrustlineLimit {
		t.Errorf("line = %s:%s limit %s", change.Line.GetCode(), change.Line.GetIssuer(), change.Limit)
	}
	// It moves nothing, so it counts for nothing against a spend ceiling.
	if spent, err := f.store.ProposedSince(ctx, f.scope.Org, view.Proposal.CreatedAt.AddDate(0, 0, -1)); err != nil || spent != 0 {
		t.Errorf("a trustline was counted as spending %s (err %v)", spent, err)
	}
	// And it is described, by the same renderer the signers' page checks.
	if view.Description == nil || !strings.Contains(view.Description.Canonical(), "EURC") {
		t.Errorf("description = %+v", view.Description)
	}
}

// TestATrustlineThatCouldNotLandIsRefusedFirst: each of these fails on the
// network at submission, after every signature has been collected.
func TestATrustlineThatCouldNotLandIsRefusedFirst(t *testing.T) {
	f := newProposalFixture(t, "trust-refused")
	ctx := context.Background()
	treasury := f.treasury.Address()
	missing := keypair.MustRandom().Address()
	f.horizon.absent = map[string]bool{missing: true}

	for name, c := range map[string]struct {
		asset string
		held  []horizon.Balance
		want  string
	}{
		"no trustline for XLM":     {"XLM", nil, "needs no trustline"},
		"a code with no issuer":    {"EURC", nil, "name its issuer"},
		"an issuer that is no key": {"EURC:GNOPE", nil, "is not an issuer"},
		"already trusted":          {"USDC", nil, "already has a trustline"},
		"its own asset":            {"OURS:" + treasury, nil, "issues OURS itself"},
		"an issuer nobody created": {"EURC:" + missing, nil, "does not exist on this network"},
		"no XLM to lock":           {"EURC:" + eurcIssuer, []horizon.Balance{xlm("1.2"), line("USDC", testIssuer, "5")}, "to spare"},
	} {
		t.Run(name, func(t *testing.T) {
			f.horizon.held = map[string][]horizon.Balance{}
			if c.held != nil {
				f.horizon.held[treasury] = c.held
			}
			_, _, err := f.svc.ProposeTrust(ctx, f.scope, f.treasuryRow.ID, c.asset, f.member)
			if reason := refusal(t, err); !strings.Contains(reason, c.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, c.want)
			}
			if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
				t.Error("a proposal was opened for a trustline that could not land")
			}
		})
	}
}

func TestUntrustOnlyClosesAnEmptyLine(t *testing.T) {
	f := newProposalFixture(t, "untrust")
	ctx := context.Background()
	treasury := f.treasury.Address()

	f.horizon.held = map[string][]horizon.Balance{treasury: {xlm("100"), line("EURC", eurcIssuer, "3.5")}}
	_, _, err := f.svc.ProposeUntrust(ctx, f.scope, f.treasuryRow.ID, "EURC", f.member)
	if reason := refusal(t, err); !strings.Contains(reason, "still holds 3.50") {
		t.Errorf("reason = %q", reason)
	}
	_, _, err = f.svc.ProposeUntrust(ctx, f.scope, f.treasuryRow.ID, "GBP:"+eurcIssuer, f.member)
	if reason := refusal(t, err); !strings.Contains(reason, "no trustline") {
		t.Errorf("reason = %q", reason)
	}

	// Empty, it closes — and a bare code is enough, because the treasury holds
	// exactly one asset called that.
	f.horizon.held[treasury] = []horizon.Balance{xlm("100"), line("EURC", eurcIssuer, "0")}
	view, name, err := f.svc.ProposeUntrust(ctx, f.scope, f.treasuryRow.ID, "eurc", f.member)
	if err != nil {
		t.Fatalf("ProposeUntrust: %v", err)
	}
	change := ops(t, view)[0].(*txnbuild.ChangeTrust)
	if change.Limit != "0.0000000" || name != "EURC:"+eurcIssuer {
		t.Errorf("limit %q, name %q; want the line closed", change.Limit, name)
	}
}

// TestABareCodeNamesOnlyWhatItCanOnlyMean: anybody can issue an asset with any
// code, so a code alone is accepted only for the deployment's own asset or one
// the account already holds — never looked up by name.
func TestABareCodeNamesOnlyWhatItCanOnlyMean(t *testing.T) {
	f := newProposalFixture(t, "asset-names")
	other := keypair.MustRandom().Address()
	held := []Holding{
		{Asset: txnbuild.CreditAsset{Code: "EURC", Issuer: eurcIssuer}},
		{Asset: txnbuild.CreditAsset{Code: "DUP", Issuer: eurcIssuer}},
		{Asset: txnbuild.CreditAsset{Code: "DUP", Issuer: other}},
	}

	for text, want := range map[string]string{
		"XLM": "XLM", "xlm": "XLM", "native": "XLM",
		"USDC": "USDC", "usdc": "USDC",
		"EURC":               "EURC:" + eurcIssuer,
		"DUP:" + other:       "DUP:" + other,
		"USDC:" + eurcIssuer: "USDC:" + eurcIssuer, // somebody else's USDC is named in full
	} {
		asset, err := f.svc.parseAsset(text, held)
		if err != nil {
			t.Errorf("parseAsset(%q): %v", text, err)
			continue
		}
		if got := f.svc.AssetName(asset); got != want {
			t.Errorf("parseAsset(%q) is %s, want %s", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"":                              "no asset",
		"GBP":                           "name its issuer",
		"DUP":                           "more than one asset called DUP",
		"E$":                            "not an asset",
		"EURC:nothere":                  "is not an issuer",
		"WAYTOOLONGCODE1:" + eurcIssuer: "not an asset code",
	} {
		_, err := f.svc.parseAsset(text, held)
		if reason := refusal(t, err); !strings.Contains(reason, want) {
			t.Errorf("parseAsset(%q): reason %q, want it to mention %q", text, reason, want)
		}
	}
}

func TestSwapIsAPathPaymentTheTreasuryMakesToItself(t *testing.T) {
	f := newProposalFixture(t, "swap")
	ctx := context.Background()
	treasury := f.treasury.Address()
	f.horizon.held = map[string][]horizon.Balance{treasury: {xlm("500"), line("USDC", testIssuer, "40")}}
	// Three paths, the best in the middle: the amount decides, not the order.
	f.horizon.market = []string{"11.9000000", "12.3400000", "12.0000000"}

	quote, err := f.svc.QuoteSwap(ctx, f.scope, f.treasuryRow.ID, money.MustParse("100"), "XLM", "USDC")
	if err != nil {
		t.Fatalf("QuoteSwap: %v", err)
	}
	if quote.Receive != money.MustParse("12.34") || quote.FromName != "XLM" || quote.ToName != "USDC" {
		t.Fatalf("quote = %+v", quote)
	}
	if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
		t.Fatal("asking for a quote opened a proposal")
	}

	view, _, floor, err := f.svc.ProposeSwap(ctx, f.scope, SwapParams{
		Treasury: f.treasuryRow.ID, Amount: money.MustParse("100"), From: "XLM", To: "USDC",
		SlippageBps: 100, CreatedBy: f.member,
	})
	if err != nil {
		t.Fatalf("ProposeSwap: %v", err)
	}
	// One percent under 12.34, truncated, never rounded up into something
	// nobody agreed to.
	if floor != money.MustParse("12.2166") {
		t.Errorf("floor = %s, want 12.2166000", floor)
	}
	got := ops(t, view)
	swap, ok := got[0].(*txnbuild.PathPaymentStrictSend)
	if len(got) != 1 || !ok || view.Proposal.Kind != "dex" {
		t.Fatalf("kind %q, operations %#v", view.Proposal.Kind, got)
	}
	if swap.Destination != treasury || swap.SendAmount != "100.0000000" || swap.DestMin != floor.String() ||
		!swap.SendAsset.IsNative() || swap.DestAsset.GetCode() != "USDC" {
		t.Errorf("swap = %+v", swap)
	}
}

func TestASwapStatesItsOwnFloor(t *testing.T) {
	f := newProposalFixture(t, "swap-floor")
	ctx := context.Background()
	f.horizon.held = map[string][]horizon.Balance{f.treasury.Address(): {xlm("500"), line("USDC", testIssuer, "40")}}
	f.horizon.market = []string{"12.3400000"}
	base := SwapParams{Treasury: f.treasuryRow.ID, Amount: money.MustParse("100"), From: "XLM", To: "USDC", CreatedBy: f.member}

	// No floor and no tolerance: there is no default, because a default would
	// be a number chosen here about somebody else's money.
	if _, _, _, err := f.svc.ProposeSwap(ctx, f.scope, base); err == nil {
		t.Fatal("a swap was proposed with no floor")
	}

	// A floor the market does not meet today would fail on the ledger and take
	// the signatures with it.
	over := base
	over.Minimum = money.MustParse("13")
	_, _, _, err := f.svc.ProposeSwap(ctx, f.scope, over)
	var moved *MarketMovedError
	if !errors.As(err, &moved) || moved.Now != money.MustParse("12.34") {
		t.Fatalf("error = %v, want the market reported as below the floor", err)
	}
	if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
		t.Fatal("a proposal was opened for a swap that would fail today")
	}

	named := base
	named.Minimum = money.MustParse("12")
	view, _, floor, err := f.svc.ProposeSwap(ctx, f.scope, named)
	if err != nil || floor != money.MustParse("12") {
		t.Fatalf("ProposeSwap: floor %s, err %v", floor, err)
	}
	if swap := ops(t, view)[0].(*txnbuild.PathPaymentStrictSend); swap.DestMin != "12.0000000" {
		t.Errorf("DestMin = %s", swap.DestMin)
	}
}

func TestASwapThatCouldNotLandIsRefusedFirst(t *testing.T) {
	f := newProposalFixture(t, "swap-refused")
	ctx := context.Background()
	treasury := f.treasury.Address()
	f.horizon.market = []string{"12.3400000"}

	for name, c := range map[string]struct {
		amount, from, to string
		held             []horizon.Balance
		want             string
	}{
		"for itself":         {"10", "XLM", "native", []horizon.Balance{xlm("500")}, "for itself"},
		"more than it holds": {"50", "USDC", "XLM", []horizon.Balance{xlm("500"), line("USDC", testIssuer, "40")}, "which is less than 50.00"},
		// XLM the network makes the account keep is not XLM it can swap.
		"the reserve":       {"100", "XLM", "USDC", []horizon.Balance{xlm("100"), line("USDC", testIssuer, "0")}, "can spare"},
		"nothing to send":   {"5", "EURC:" + eurcIssuer, "XLM", []horizon.Balance{xlm("500")}, "holds no EURC"},
		"nowhere to put it": {"5", "XLM", "EURC:" + eurcIssuer, []horizon.Balance{xlm("500")}, "/trust EURC:" + eurcIssuer},
	} {
		t.Run(name, func(t *testing.T) {
			f.horizon.held = map[string][]horizon.Balance{treasury: c.held}
			_, err := f.svc.QuoteSwap(ctx, f.scope, f.treasuryRow.ID, money.MustParse(c.amount), c.from, c.to)
			if reason := refusal(t, err); !strings.Contains(reason, c.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, c.want)
			}
		})
	}

	// And with nobody offering the exchange, that is the answer.
	f.horizon.held = map[string][]horizon.Balance{treasury: {xlm("500"), line("USDC", testIssuer, "40")}}
	f.horizon.market = nil
	if _, err := f.svc.QuoteSwap(ctx, f.scope, f.treasuryRow.ID, money.MustParse("10"), "XLM", "USDC"); !errors.Is(err, settlement.ErrNoPath) {
		t.Fatalf("error = %v, want ErrNoPath", err)
	}
}

// TestASwapOutOfTheDeploymentsAssetCountsAgainstItsCeilings: it moves that
// asset out of the treasury, which is what the ceilings bound.
func TestASwapOutOfTheDeploymentsAssetCountsAgainstItsCeilings(t *testing.T) {
	f := newProposalFixture(t, "swap-ceiling")
	ctx := context.Background()
	f.horizon.held = map[string][]horizon.Balance{f.treasury.Address(): {xlm("500"), line("USDC", testIssuer, "4000")}}
	f.horizon.market = []string{"900.0000000"}
	if err := f.store.SetSpendPolicy(ctx, f.scope.Org, store.SpendPolicy{PerPayment: money.MustParse("100")}); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}

	_, _, _, err := f.svc.ProposeSwap(ctx, f.scope, SwapParams{
		Treasury: f.treasuryRow.ID, Amount: money.MustParse("500"), From: "USDC", To: "XLM",
		Minimum: money.MustParse("800"), CreatedBy: f.member,
	})
	var over *CeilingError
	if !errors.As(err, &over) {
		t.Fatalf("error = %v, want the workspace's ceiling", err)
	}

	// Into it is not spending it.
	if _, _, _, err := f.svc.ProposeSwap(ctx, f.scope, SwapParams{
		Treasury: f.treasuryRow.ID, Amount: money.MustParse("400"), From: "XLM", To: "USDC",
		Minimum: money.MustParse("800"), CreatedBy: f.member,
	}); err != nil {
		t.Fatalf("a swap into the asset was held to its spend ceiling: %v", err)
	}
}

// TestASwapTheMarketHasLeftIsNotSubmitted: a path payment that cannot meet its
// minimum fails on the ledger, which spends the sequence number, closes the
// proposal and throws away every signature on it. Waiting costs nothing.
func TestASwapTheMarketHasLeftIsNotSubmitted(t *testing.T) {
	f := newProposalFixture(t, "swap-moved")
	ctx := context.Background()
	f.horizon.held = map[string][]horizon.Balance{f.treasury.Address(): {xlm("500"), line("USDC", testIssuer, "40")}}
	f.horizon.market = []string{"12.3400000"}

	view, _, _, err := f.svc.ProposeSwap(ctx, f.scope, SwapParams{
		Treasury: f.treasuryRow.ID, Amount: money.MustParse("100"), From: "XLM", To: "USDC",
		Minimum: money.MustParse("12"), CreatedBy: f.member,
	})
	if err != nil {
		t.Fatalf("ProposeSwap: %v", err)
	}
	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury, f.co), f.identity); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	for name, market := range map[string][]string{"below the floor": {"11.0000000"}, "gone": nil} {
		f.horizon.market = market
		before := len(f.horizon.submitted)
		_, err := f.svc.ExecuteProposal(ctx, f.scope, view.Proposal.ID, f.identity)
		var moved *MarketMovedError
		if !errors.As(err, &moved) || moved.Minimum != money.MustParse("12") {
			t.Fatalf("%s: error = %v, want the market reported as moved", name, err)
		}
		if len(f.horizon.submitted) != before {
			t.Fatalf("%s: a swap that would fail was submitted anyway", name)
		}
		after, err := f.store.Proposal(ctx, f.scope.Org, view.Proposal.ID)
		if err != nil || after.Status != store.ProposalOpen {
			t.Fatalf("%s: the proposal is %q (err %v), want it left open with its signatures", name, after.Status, err)
		}
	}

	// Back above the floor, the same press goes through.
	f.horizon.market = []string{"12.1000000"}
	if _, err := f.svc.ExecuteProposal(ctx, f.scope, view.Proposal.ID, f.identity); err != nil {
		t.Fatalf("Execute once the market was back: %v", err)
	}
}

func TestBalancesAreReadFromTheNetwork(t *testing.T) {
	f := newProposalFixture(t, "balances")
	ctx := context.Background()
	mine := keypair.MustRandom().Address()
	unfunded := keypair.MustRandom().Address()
	frozen := false
	f.horizon.absent = map[string]bool{unfunded: true}
	f.horizon.held = map[string][]horizon.Balance{
		f.treasury.Address(): {
			xlm("12.5"), line("USDC", testIssuer, "1000"),
			{Balance: "3", Limit: "10", IsAuthorized: &frozen, Asset: base.Asset{Type: "credit_alphanum4", Code: "EURC", Issuer: eurcIssuer}},
		},
		mine: {xlm("2")},
	}

	report, err := f.svc.Balances(ctx, f.scope, true, mine)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	if len(report.Treasuries) != 1 || len(report.Wallets) != 1 {
		t.Fatalf("report = %+v", report)
	}
	tr := report.Treasuries[0]
	want := map[string]string{"XLM": "12.5", "USDC": "1000", "EURC:" + eurcIssuer: "3"}
	for _, l := range tr.Lines {
		if l.Amount != money.MustParse(want[l.Asset]) {
			t.Errorf("%s = %s, want %s", l.Asset, l.Amount, want[l.Asset])
		}
		if (l.Asset == "EURC:"+eurcIssuer) == l.Authorized {
			t.Errorf("%s authorized = %v", l.Asset, l.Authorized)
		}
		delete(want, l.Asset)
	}
	if len(want) != 0 || tr.Label != "main" {
		t.Errorf("the treasury is missing %v (label %q)", want, tr.Label)
	}

	// Somebody without standing is shown their own and not the treasury's.
	report, err = f.svc.Balances(ctx, f.scope, false, unfunded)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	if len(report.Treasuries) != 0 {
		t.Errorf("treasuries were read for somebody they are not shown to: %+v", report.Treasuries)
	}
	// An address with a key and no account is that, and not a zero balance.
	if len(report.Wallets) != 1 || report.Wallets[0].Exists || len(report.Wallets[0].Lines) != 0 {
		t.Errorf("an unfunded wallet was reported as %+v", report.Wallets)
	}
}
