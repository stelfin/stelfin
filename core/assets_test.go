package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/settlement"
)

var spaceSeq int

// nextSpace is a chat space no other test in this file has set up. Each case
// links a treasury, and two in one workspace is a different situation from the
// one being tested.
func nextSpace() string {
	spaceSeq++
	return fmt.Sprintf("-4709%04d", spaceSeq)
}

func holdings() *api.BalanceReport {
	return &api.BalanceReport{
		Treasuries: []api.AccountBalance{{
			Label: "main", Address: "GTREASURY", Exists: true,
			Lines: []api.BalanceLine{
				{Asset: "XLM", Amount: money.MustParse("12.5"), Authorized: true},
				{Asset: "USDC", Amount: money.MustParse("1000"), Authorized: true},
				{Asset: "EURC:GISSUER", Amount: money.MustParse("3"), Authorized: false},
			},
		}},
		Wallets: []api.AccountBalance{{Label: "linked", Address: "GMINE", Exists: false}},
	}
}

// TestBalanceShowsWhatIsHeld: a treasury bot that cannot say what the treasury
// holds sends everybody to a block explorer for the one number they came for.
func TestBalanceShowsWhatIsHeld(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-470001")
	h.sender.balances = holdings()
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-470001", "balance", "")); err != nil {
		t.Fatalf("balance: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{
		"Treasury — main", "GTREASURY", "1,000.00 USDC", "12.50 XLM",
		// The whole name, where a code alone would read the same for two assets.
		"EURC:GISSUER", "frozen by its issuer",
		// An address with no account behind it is said to be that, not shown as zero.
		"GMINE", "not on the network yet",
		"Read from the network just now",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}
}

// TestBalanceShowsATreasuryOnlyToSomeoneWithARole: the bot answers the people
// it has been told about.
func TestBalanceShowsATreasuryOnlyToSomeoneWithARole(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-470002")
	h.sender.balances = holdings()

	stranger := message("-470002", "balance", "")
	stranger.Actor.UserID, stranger.Actor.Handle = "99", "nobody"
	h.out = &fakeReplier{}
	if err := h.handle(t, stranger); err != nil {
		t.Fatalf("balance: %v", err)
	}
	body := h.out.only(t).Text
	if strings.Contains(body, "GTREASURY") || strings.Contains(body, "1,000.00") {
		t.Errorf("the treasury was shown to somebody with no role:\n%s", body)
	}
	if !strings.Contains(body, "GMINE") || !strings.Contains(body, "/grant") {
		t.Errorf("their own wallet, or how to be shown more, is missing:\n%s", body)
	}
	if got := h.sender.balanceAsked[len(h.sender.balanceAsked)-1]; !strings.HasPrefix(got, "treasuries=false") {
		t.Errorf("the treasury was read for somebody with no role: %s", got)
	}
}

func TestTrustOpensAProposal(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-470003")

	if err := h.handle(t, message("-470003", "trust", "EURC:GISSUER")); err != nil {
		t.Fatalf("trust: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{"Proposal #", "EURC:GISSUER", "0.5 XLM", "/approve#", "/cancel "} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-470003", "trust", "")); err != nil {
		t.Fatalf("trust with no asset: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "issuer") {
		t.Errorf("the usage does not say an issuer is needed:\n%s", body)
	}
}

// TestAProposalThatCouldNotLandIsNotPutToSigners: found before anybody signs,
// and said in the words the service gave for it.
func TestAProposalThatCouldNotLandIsNotPutToSigners(t *testing.T) {
	for name, c := range map[string]struct {
		command, args string
		err           error
		want          string
	}{
		"already trusted":  {"trust", "EURC:GISSUER", &api.RefusalError{Reason: "the treasury already has a trustline for EURC:GISSUER"}, "already has a trustline"},
		"still holds some": {"untrust", "EURC", &api.RefusalError{Reason: "the treasury still holds 3.00 EURC"}, "still holds 3.00"},
		"no order book":    {"swap", "100 XLM to USDC", settlement.ErrNoPath, "Nobody on this network is offering"},
		"no trustline":     {"swap", "100 XLM to EURC:GISSUER min:5", &api.RefusalError{Reason: "the treasury has no trustline for EURC:GISSUER"}, "no trustline"},
		"over a ceiling":   {"swap", "9000 USDC to XLM min:1", &api.CeilingError{Scope: "workspace", Limit: money.MustParse("100")}, "/limits"},
		"market below the floor": {
			"swap", "100 XLM to USDC min:50",
			&api.MarketMovedError{Now: money.MustParse("12"), Minimum: money.MustParse("50"), Asset: "USDC"},
			"will not take less than 50.00",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, true)
			space := nextSpace()
			_, tr := proposerIn(t, h, space)
			h.sender.assetErr = c.err

			if err := h.handle(t, message(space, c.command, c.args)); err != nil {
				t.Fatalf("%s returned an error, so the member would be told nothing useful: %v", c.command, err)
			}
			body := h.out.only(t).Text
			if !strings.Contains(body, c.want) {
				t.Errorf("reply does not mention %q:\n%s", c.want, body)
			}
			if strings.Contains(body, "/approve#") {
				t.Errorf("a link was handed out for something that was refused:\n%s", body)
			}
			if _, open, _ := h.store.OpenProposal(context.Background(), tr.Org, tr.ID); open {
				t.Error("a proposal was opened for something that was refused")
			}
		})
	}
}

// TestSwapQuotesBeforeItProposes: the floor is the whole of a swap's risk, and
// nobody can choose it without seeing what the market offers. So asking with no
// floor gets the market's answer and proposes nothing.
func TestSwapQuotesBeforeItProposes(t *testing.T) {
	h := newHarness(t, true)
	_, tr := proposerIn(t, h, "-470020")
	h.sender.market = money.MustParse("12.34")

	if err := h.handle(t, message("-470020", "swap", "100 XLM to USDC")); err != nil {
		t.Fatalf("swap: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{"100.00 XLM", "about 12.34 USDC", "Nothing has been proposed", "min:", "slippage:1%"} {
		if !strings.Contains(body, want) {
			t.Errorf("the quote does not mention %q:\n%s", want, body)
		}
	}
	if len(h.sender.swaps) != 0 {
		t.Fatalf("a swap was proposed with no floor named: %+v", h.sender.swaps)
	}
	if _, open, _ := h.store.OpenProposal(context.Background(), tr.Org, tr.ID); open {
		t.Fatal("asking for a quote opened a proposal")
	}

	// With a floor, it is a proposal, and the floor is the one that was named.
	for args, want := range map[string]api.SwapParams{
		"100 XLM to USDC min:12":       {Minimum: money.MustParse("12")},
		"100 XLM for USDC slippage:1%": {SlippageBps: 100},
		"100 XLM USDC slippage:0.5":    {SlippageBps: 50},
	} {
		h := newHarness(t, true)
		space := nextSpace()
		proposerIn(t, h, space)
		h.sender.market = money.MustParse("12.34")
		if err := h.handle(t, message(space, "swap", args)); err != nil {
			t.Fatalf("swap %q: %v", args, err)
		}
		body := h.out.only(t).Text
		if !strings.Contains(body, "Proposal #") || !strings.Contains(body, "at least") {
			t.Errorf("swap %q did not open a proposal that states its floor:\n%s", args, body)
		}
		got := h.sender.swaps[0]
		if got.Amount != money.MustParse("100") || got.From != "XLM" || got.To != "USDC" ||
			got.Minimum != want.Minimum || got.SlippageBps != want.SlippageBps {
			t.Errorf("swap %q was proposed as %+v", args, got)
		}
	}
}

func TestSwapRefusesAFloorItCannotRead(t *testing.T) {
	for args, want := range map[string]string{
		"":                                   "Try: /swap",
		"lots XLM to USDC":                   "as an amount",
		"100 XLM to USDC min:plenty":         "least you'd accept",
		"100 XLM to USDC slippage:90%":       "tolerance",
		"100 XLM to USDC slippage:0":         "tolerance",
		"100 XLM to USDC min:12 slippage:1%": "one or the other",
	} {
		h := newHarness(t, true)
		space := nextSpace()
		proposerIn(t, h, space)
		if err := h.handle(t, message(space, "swap", args)); err != nil {
			t.Fatalf("swap %q: %v", args, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, want) {
			t.Errorf("swap %q: reply does not mention %q:\n%s", args, want, body)
		}
		if len(h.sender.swaps) != 0 || h.sender.quoted != 0 {
			t.Errorf("swap %q reached the market", args)
		}
	}
}

func TestSwapArguments(t *testing.T) {
	issued := "EURC:GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	for args, want := range map[string]map[string]string{
		"100 XLM to USDC":                       {"amount": "100", "from": "XLM", "to": "USDC"},
		"100 XLM USDC":                          {"amount": "100", "from": "XLM", "to": "USDC"},
		"2.5 " + issued + " to XLM min:1":       {"amount": "2.5", "from": issued, "to": "XLM", "min": "1"},
		"100 XLM to " + issued + " slippage:2%": {"amount": "100", "from": "XLM", "to": issued, "slippage": "2%"},
	} {
		got := swapArguments(chat.Inbound{Args: args})
		for k, v := range want {
			if got[k] != v {
				t.Errorf("swapArguments(%q)[%s] = %q, want %q", args, k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("swapArguments(%q) = %v, want %v", args, got, want)
		}
	}
	// Discord's options win over anything in the text.
	got := swapArguments(chat.Inbound{Args: "ignored", Options: map[string]string{"amount": "5", "from": "XLM", "to": "USDC"}})
	if got["amount"] != "5" || got["from"] != "XLM" {
		t.Errorf("options were not read: %v", got)
	}
}

func TestBasisPoints(t *testing.T) {
	for text, want := range map[string]int64{"1%": 100, "1": 100, "0.5%": 50, "2.25": 225, " 10% ": 1000} {
		if got, err := basisPoints(text); err != nil || got != want {
			t.Errorf("basisPoints(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	// A tolerance of half the value or more is a typo, and this is the one
	// place a typo costs money.
	for _, text := range []string{"0", "-1", "50", "100%", "lots", "", "0.001"} {
		if got, err := basisPoints(text); err == nil {
			t.Errorf("basisPoints(%q) = %d, want it refused", text, got)
		}
	}
}
