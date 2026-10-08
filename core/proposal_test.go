package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// payMessage builds a /pay with its options filled in, which is how it arrives
// from Discord. Telegram delivers the same command as text; see
// TestPayReadsItsArgumentsFromText.
func payMessage(space, amount, to string) chat.Inbound {
	m := message(space, "pay", "")
	m.Options = map[string]string{"amount": amount, "to": to}
	return m
}

// proposerIn sets a space up, links a treasury, and gives the speaker the role
// /pay needs.
func proposerIn(t *testing.T, h *harness, space string) (store.Org, store.Treasury) {
	t.Helper()
	org := setUpWorkspace(t, h, space)
	tr := treasuryFor(t, h, org)
	grantRole(t, h, org, "42", chat.RoleProposer)
	h.out = &fakeReplier{}
	return org, tr
}

func TestPayOpensAProposalAndHandsOutALink(t *testing.T) {
	h := newHarness(t, true)
	_, tr := proposerIn(t, h, "-420001")
	to := keypair.MustRandom().Address()

	if err := h.handle(t, payMessage("-420001", "250", to)); err != nil {
		t.Fatalf("pay: %v", err)
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "/approve#") {
		t.Errorf("reply does not carry a signing link:\n%s", body)
	}
	// The two things the proposer has to pass on: everyone else asks for their
	// own link, and anyone can submit once it has the weight.
	if !strings.Contains(body, "/approve ") {
		t.Errorf("reply does not tell other signers how to get a link:\n%s", body)
	}
	if !strings.Contains(body, "/execute ") {
		t.Errorf("reply does not say how it gets submitted:\n%s", body)
	}

	open, ok, err := h.store.OpenProposal(context.Background(), tr.Org, tr.ID)
	if err != nil || !ok {
		t.Fatalf("open proposal: %v (found %v)", err, ok)
	}
	if open.Kind != "payment" {
		t.Errorf("kind = %q", open.Kind)
	}
}

// TestPayReadsItsArgumentsFromText: Telegram has no structured options, so a
// /pay that read only those could answer nothing but its own usage line there.
func TestPayReadsItsArgumentsFromText(t *testing.T) {
	h := newHarness(t, true)
	_, tr := proposerIn(t, h, "-429001")
	to := keypair.MustRandom().Address()

	if err := h.handle(t, message("-429001", "pay", "250 to "+to+" march invoice")); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/approve#") {
		t.Fatalf("reply does not carry a signing link:\n%s", body)
	}
	if _, ok, err := h.store.OpenProposal(context.Background(), tr.Org, tr.ID); err != nil || !ok {
		t.Fatalf("open proposal: %v (found %v)", err, ok)
	}
}

func TestPayArguments(t *testing.T) {
	cases := map[string]struct {
		args string
		want map[string]string
	}{
		"positional":        {"250 GABC", map[string]string{"amount": "250", "to": "GABC"}},
		"with to":           {"250 to GABC", map[string]string{"amount": "250", "to": "GABC"}},
		"with a memo":       {"250 GABC march invoice", map[string]string{"amount": "250", "to": "GABC", "memo": "march invoice"}},
		"named":             {"amount:250 to:GABC", map[string]string{"amount": "250", "to": "GABC"}},
		"named, any order":  {"to:GABC amount:250", map[string]string{"amount": "250", "to": "GABC"}},
		"named memo":        {"250 GABC memo:for the audit", map[string]string{"amount": "250", "to": "GABC", "memo": "for the audit"}},
		"a named treasury":  {"250 GABC treasury:ops", map[string]string{"amount": "250", "to": "GABC", "treasury": "ops"}},
		"nothing":           {"", map[string]string{}},
		"an amount, alone":  {"250", map[string]string{"amount": "250"}},
		"to with no target": {"250 to", map[string]string{"amount": "250"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := payArguments(chat.Inbound{Command: "pay", Args: tc.args})
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("payArguments(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}

	// Structured options win outright: a platform that delivers them has
	// already said what each value is.
	got := payArguments(chat.Inbound{
		Options: map[string]string{"amount": "5", "to": "GDEF"}, Args: "999 GABC",
	})
	if got["amount"] != "5" || got["to"] != "GDEF" {
		t.Errorf("options did not take precedence: %v", got)
	}
}

// TestPaySaysWhyASecondProposalIsRefused: two open envelopes from one account
// reserve the same sequence number, so whichever executes first silently kills
// the other. That is worth explaining rather than reporting as a conflict.
func TestPaySaysWhyASecondProposalIsRefused(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420002")
	to := keypair.MustRandom().Address()

	if err := h.handle(t, payMessage("-420002", "10", to)); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, payMessage("-420002", "20", to)); err != nil {
		t.Fatalf("second pay: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "sequence number") {
		t.Errorf("refusal does not say why one at a time:\n%s", body)
	}
	if !strings.Contains(body, "already has proposal #") {
		t.Errorf("refusal does not name the open one:\n%s", body)
	}
}

func TestPayNeedsATreasuryFirst(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-420003")
	grantRole(t, h, org, "42", chat.RoleProposer)
	h.out = &fakeReplier{}

	if err := h.handle(t, payMessage("-420003", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/link-treasury") {
		t.Errorf("reply does not say what to do first:\n%s", body)
	}
}

func TestPayRefusesWhatItCannotSend(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420004")

	for name, m := range map[string]chat.Inbound{
		"no amount":      payMessage("-420004", "", keypair.MustRandom().Address()),
		"no destination": payMessage("-420004", "10", ""),
		"bad amount":     payMessage("-420004", "a lot", keypair.MustRandom().Address()),
		"zero":           payMessage("-420004", "0", keypair.MustRandom().Address()),
		"not an account": payMessage("-420004", "10", "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"),
	} {
		t.Run(name, func(t *testing.T) {
			h.out = &fakeReplier{}
			if err := h.handle(t, m); err != nil {
				t.Fatalf("pay: %v", err)
			}
			if body := h.out.only(t).Text; strings.Contains(body, "/approve#") {
				t.Fatalf("a bad request produced a signing link:\n%s", body)
			}
		})
	}

	// And nothing was opened by any of them.
	list, err := h.store.Proposals(context.Background(), h.orgOf(t, "-420004").ID, 10)
	if err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d proposal(s) opened by requests that should have been refused", len(list))
	}
}

// TestApproveIsNotGatedOnAPlatformRole: the account's signer list decides
// whether a signature counts. A platform role standing in for that is the bug
// that loses a treasury, so this hands out a link to anyone the org knows.
func TestApproveIsNotGatedOnAPlatformRole(t *testing.T) {
	h := newHarness(t, true)
	org, _ := proposerIn(t, h, "-420005")

	if err := h.handle(t, payMessage("-420005", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}

	grantRole(t, h, org, "77", chat.RoleObserver)
	h.out = &fakeReplier{}

	if err := h.handle(t, messageFrom("77", "-420005", "approve", "")); err != nil {
		t.Fatalf("approve: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "/approve#") {
		t.Errorf("an observer was not given a link:\n%s", body)
	}
	if !strings.Contains(body, "0 of 2") {
		t.Errorf("reply does not say where the proposal stands:\n%s", body)
	}
}

// TestApproveWithNoNumberFindsTheOpenOne: a treasury can only have one open, so
// naming it adds nothing but a chance to mistype it.
func TestApproveWithNoNumberFindsTheOpenOne(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420006")

	if err := h.handle(t, payMessage("-420006", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	open, ok, err := h.store.OpenProposal(context.Background(),
		h.orgOf(t, "-420006").ID, h.treasuryOf(t, "-420006").ID)
	if err != nil || !ok {
		t.Fatalf("open proposal: %v (found %v)", err, ok)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420006", "approve", "")); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, fmt.Sprintf("#%d", open.ID)) {
		t.Errorf("reply does not name the open proposal:\n%s", body)
	}
}

func TestApproveRefusesAProposalFromAnotherWorkspace(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420007")
	proposerIn(t, h, "-420008")

	if err := h.handle(t, payMessage("-420007", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	open, _, err := h.store.OpenProposal(context.Background(),
		h.orgOf(t, "-420007").ID, h.treasuryOf(t, "-420007").ID)
	if err != nil {
		t.Fatalf("open proposal: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420008", "approve", fmt.Sprint(open.ID))); err != nil {
		t.Fatalf("approve: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "no proposal") {
		t.Errorf("another workspace's proposal was not refused:\n%s", body)
	}
	if strings.Contains(body, "/approve#") {
		t.Fatalf("another workspace's proposal produced a signing link:\n%s", body)
	}
}

// TestExecuteExplainsAMovedSequence is the silent killer of multisig said out
// loud: no set of signatures can make that envelope valid again, and the member
// needs to know nothing was paid twice.
func TestExecuteExplainsAMovedSequence(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420009")

	if err := h.handle(t, payMessage("-420009", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.sender.executeErr = fmt.Errorf("%w: it moved", api.ErrSequenceMoved)
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420009", "execute", "")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{"sequence number", "nothing is lost", "/pay"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}
}

func TestExecuteNamesWhoIsStillMissing(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420010")

	if err := h.handle(t, payMessage("-420010", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.sender.executeErr = fmt.Errorf("%w: 0 of 2", api.ErrNotEnoughSignatures)
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420010", "execute", "")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	body := h.out.only(t).Text
	// A weight alone leaves the channel guessing which of five people to chase.
	if !strings.Contains(body, "GSIGNER-ONE") || !strings.Contains(body, "GSIGNER-TWO") {
		t.Errorf("reply does not name who is missing:\n%s", body)
	}
}

func TestExecuteReportsTheTransaction(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420011")

	if err := h.handle(t, payMessage("-420011", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420011", "execute", "")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "ledger 42") {
		t.Errorf("reply does not report the transaction:\n%s", body)
	}
	if len(h.sender.executed) != 1 {
		t.Fatalf("executed %d proposal(s)", len(h.sender.executed))
	}
}

func TestProposalsLists(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-420012")

	if err := h.handle(t, message("-420012", "proposals", "")); err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/pay") {
		t.Errorf("the empty case does not say how to start one:\n%s", body)
	}

	if err := h.handle(t, payMessage("-420012", "10", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-420012", "proposals", "")); err != nil {
		t.Fatalf("proposals: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "payment") || !strings.Contains(body, "open") {
		t.Errorf("listing does not describe the proposal:\n%s", body)
	}
}

// TestAFailedCommandIsNotSilence: an error that reaches only the log looks,
// from the chat, exactly like a bot that is not there. The member is told it
// failed and that nothing changed — and none of the error's own text, which can
// carry addresses and upstream URLs.
func TestAFailedCommandIsNotSilence(t *testing.T) {
	h := newHarness(t, true)
	proposerIn(t, h, "-429002")
	to := keypair.MustRandom().Address()
	if err := h.handle(t, payMessage("-429002", "10", to)); err != nil {
		t.Fatalf("pay: %v", err)
	}
	h.out = &fakeReplier{}

	h.sender.executeErr = errors.New(`load account GABC: Get "https://horizon.example/accounts/GABC": TLS handshake timeout`)
	err := h.handle(t, message("-429002", "execute", ""))
	if err == nil {
		t.Fatal("the failure was swallowed; it has to reach the log")
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "Nothing was sent or changed") {
		t.Errorf("reply does not say what state things are in:\n%s", body)
	}
	for _, leak := range []string{"horizon.example", "GABC", "TLS"} {
		if strings.Contains(body, leak) {
			t.Errorf("reply leaks %q from the internal error:\n%s", leak, body)
		}
	}
}
