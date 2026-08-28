package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// setUpWorkspace registers a space and returns the org behind it.
func setUpWorkspace(t *testing.T, h *harness, space string) store.Org {
	t.Helper()
	if err := h.handle(t, message(space, commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	org, ok, err := h.store.OrgForSpace(context.Background(), chat.Telegram, space)
	if err != nil || !ok {
		t.Fatalf("org for space: %v (found %v)", err, ok)
	}
	h.out = &fakeReplier{}
	return org
}

func TestLinkIssuesAChallenge(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400001")
	address := keypair.MustRandom().Address()

	if err := h.handle(t, message("-400001", "link", address)); err != nil {
		t.Fatalf("link: %v", err)
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "/link#") {
		t.Errorf("reply does not carry a signing link:\n%s", body)
	}
	if !strings.Contains(body, address) {
		t.Errorf("reply does not name the address:\n%s", body)
	}
	// The reason it is safe to sign has to be said, not assumed.
	if !strings.Contains(body, "never be") {
		t.Errorf("reply does not explain why signing is safe:\n%s", body)
	}
	if got := h.sender.linkedAddresses(); len(got) != 1 || got[0] != address {
		t.Errorf("challenge requested for %v, want %q", got, address)
	}
}

// TestLinkRefusesWhatItCannotProve: muxed and contract addresses land here too,
// and the message says why rather than pretending they were mistyped.
func TestLinkRefusesWhatItCannotProve(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400002")

	for _, bad := range []string{
		"MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK",
		"CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE",
		"GABC",
		"not-an-address",
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-400002", "link", bad)); err != nil {
			t.Fatalf("link %q: %v", bad, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, "G…") {
			t.Errorf("link %q reply does not explain what is needed:\n%s", bad, body)
		}
	}
	if got := h.sender.linkedAddresses(); len(got) != 0 {
		t.Errorf("challenges were issued for %v", got)
	}
}

func TestLinkNeedsAnAddress(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400003")

	if err := h.handle(t, message("-400003", "link", "  ")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "Which address") {
		t.Errorf("reply does not ask for one:\n%s", body)
	}
}

// TestLinkSaysSoWhenUnavailable: a deployment with no web-auth key cannot prove
// anything, and should say that rather than fail obscurely.
func TestLinkSaysSoWhenUnavailable(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400004")
	h.svc.challenges = nil

	if err := h.handle(t, message("-400004", "link", keypair.MustRandom().Address())); err != nil {
		t.Fatalf("link: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "isn't available") {
		t.Errorf("reply does not explain:\n%s", body)
	}
}

// TestLinkCodeNeedsAProvedMember: a code attaches a handle to a member. If that
// member has proved nothing, the code is a way to impersonate someone who is not
// yet anyone.
func TestLinkCodeNeedsAProvedMember(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400005")

	if err := h.handle(t, message("-400005", "link-code", "")); err != nil {
		t.Fatalf("link-code: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/link") {
		t.Errorf("reply does not point at linking first:\n%s", body)
	}
}

func TestLinkCodeAndClaim(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	org := setUpWorkspace(t, h, "-400006")

	// The member has proved an address, which is what a code attaches to.
	member, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "42")
	if err != nil || !ok {
		t.Fatalf("member: %v (found %v)", err, ok)
	}
	address := keypair.MustRandom().Address()
	if err := h.store.SetMemberAddress(ctx, org.ID, member.ID, address, store.AddressLinked); err != nil {
		t.Fatalf("set address: %v", err)
	}

	if err := h.handle(t, message("-400006", "link-code", "")); err != nil {
		t.Fatalf("link-code: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "/claim ") {
		t.Fatalf("reply does not carry a code:\n%s", body)
	}
	code := extractCode(t, body)

	// A second chat account, in the same workspace, claiming it.
	h.out = &fakeReplier{}
	second := message("-400006", "claim", code)
	second.Actor.UserID = "99"
	second.Actor.Handle = "ada-phone"
	if err := h.handle(t, second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if reply := h.out.only(t).Text; !strings.Contains(reply, address) {
		t.Errorf("reply does not confirm the wallet:\n%s", reply)
	}

	// One human, one member, two handles.
	viaSecond, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "99")
	if err != nil || !ok {
		t.Fatalf("second identity: %v (found %v)", err, ok)
	}
	if viaSecond.ID != member.ID {
		t.Errorf("second account speaks for member %d, want %d", viaSecond.ID, member.ID)
	}
}

// TestClaimRefusesAnAlreadyProvedAccount: claiming would silently move this
// account to a different member, which a mistyped command must not do.
func TestClaimRefusesAnAlreadyProvedAccount(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	org := setUpWorkspace(t, h, "-400007")

	member, _, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "42")
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if err := h.store.SetMemberAddress(ctx, org.ID, member.ID,
		keypair.MustRandom().Address(), store.AddressLinked); err != nil {
		t.Fatalf("set address: %v", err)
	}

	if err := h.handle(t, message("-400007", "claim", "ABCD2345")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "already speaks") {
		t.Errorf("reply does not explain:\n%s", body)
	}
}

func TestClaimRefusesAnUnknownCode(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400008")

	if err := h.handle(t, message("-400008", "claim", "ZZZZ2345")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "isn't valid") {
		t.Errorf("reply does not explain:\n%s", body)
	}
}

func TestClaimNeedsACode(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-400009")

	if err := h.handle(t, message("-400009", "claim", "")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "Which code") {
		t.Errorf("reply does not ask for one:\n%s", body)
	}
}

// extractCode pulls the code out of the reply, the way a person would read it.
func extractCode(t *testing.T, body string) string {
	t.Helper()
	const marker = "/claim "
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no code in:\n%s", body)
	}
	rest := body[i+len(marker):]
	end := strings.IndexAny(rest, "\n ")
	if end < 0 {
		end = len(rest)
	}
	code := strings.TrimSpace(rest[:end])
	if len(code) != store.LinkCodeLength {
		t.Fatalf("code %q is %d characters, want %d", code, len(code), store.LinkCodeLength)
	}
	return code
}
