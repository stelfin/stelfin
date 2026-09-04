package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// messageFrom is `message` spoken by somebody other than the workspace's first
// admin, which is who setUpWorkspace leaves behind.
func messageFrom(userID, space, command, args string) chat.Inbound {
	m := message(space, command, args)
	m.Actor = chat.Actor{Channel: chat.Telegram, UserID: userID, Handle: "bo"}
	return m
}

func TestLinkTreasuryIssuesAChallenge(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-410001")
	address := keypair.MustRandom().Address()

	if err := h.handle(t, message("-410001", "link-treasury", address)); err != nil {
		t.Fatalf("link-treasury: %v", err)
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "/link#") {
		t.Errorf("reply does not carry a signing link:\n%s", body)
	}
	if !strings.Contains(body, address) {
		t.Errorf("reply does not name the account:\n%s", body)
	}
	// The two things a signer has to know before opening it: several people
	// sign the same envelope, and signing cannot move anything.
	if !strings.Contains(body, "same envelope") {
		t.Errorf("reply does not say the signatures go on one envelope:\n%s", body)
	}
	if !strings.Contains(body, "cannot move anything") {
		t.Errorf("reply does not explain why signing is safe:\n%s", body)
	}
	if got := h.sender.provedTreasuries(); len(got) != 1 || got[0] != address {
		t.Errorf("challenge requested for %v, want %q", got, address)
	}
}

// TestLinkTreasuryIsAdminOnly: the role confers no authority over the money —
// that stays with the account's signers — but it decides who can point the
// workspace at an account, which is what later commands spend against.
func TestLinkTreasuryIsAdminOnly(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-410002")
	address := keypair.MustRandom().Address()

	if err := h.handle(t, messageFrom("99", "-410002", "link-treasury", address)); err != nil {
		t.Fatalf("link-treasury: %v", err)
	}
	if got := h.sender.provedTreasuries(); len(got) != 0 {
		t.Fatalf("a non-admin started a treasury proof: %v", got)
	}
	// Refused for the stated reason, not silent for some other one.
	if body := h.out.only(t).Text; !strings.Contains(body, "admin role") {
		t.Errorf("refusal does not say why:\n%s", body)
	}
}

// TestLinkTreasuryNamesTheContractCase: a C-address is where the product is
// going, and a flat "not an address" would read as a typo.
func TestLinkTreasuryNamesTheContractCase(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-410003")

	const contract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"
	if err := h.handle(t, message("-410003", "link-treasury", contract)); err != nil {
		t.Fatalf("link-treasury: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "contract account") {
		t.Errorf("reply does not say what a contract account is:\n%s", body)
	}
	if got := h.sender.provedTreasuries(); len(got) != 0 {
		t.Fatalf("a contract address started a signature proof: %v", got)
	}
}

// TestLinkTreasuryExplainsAnUnprovableAccount: an account whose signers cannot
// reach its own threshold cannot prove control and cannot spend either, and
// that second half is the part worth saying.
func TestLinkTreasuryExplainsAnUnprovableAccount(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-410004")
	h.sender.treasuryErr = fmt.Errorf("%w: locked out", api.ErrTreasuryUnprovable)

	address := keypair.MustRandom().Address()
	if err := h.handle(t, message("-410004", "link-treasury", address)); err != nil {
		t.Fatalf("link-treasury: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "can't prove control") {
		t.Errorf("reply does not say the proof is impossible:\n%s", body)
	}
	if !strings.Contains(body, "nobody can spend") {
		t.Errorf("reply does not say the account cannot be spent from either:\n%s", body)
	}
}

func TestTreasuriesLists(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-410005")
	ctx := context.Background()

	if err := h.handle(t, message("-410005", "treasuries", "")); err != nil {
		t.Fatalf("treasuries: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/link-treasury") {
		t.Errorf("the empty case does not say how to fix it:\n%s", body)
	}
	h.out = &fakeReplier{}

	address := keypair.MustRandom().Address()
	if _, err := h.store.LinkTreasury(ctx, store.LinkTreasuryParams{
		Org: org.ID, Kind: store.TreasuryClassic, Address: address,
		Label: "ops", Low: 1, Medium: 2, High: 2,
	}); err != nil {
		t.Fatalf("link treasury: %v", err)
	}

	if err := h.handle(t, message("-410005", "treasuries", "")); err != nil {
		t.Fatalf("treasuries: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{address, "ops", "weight 2"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing does not mention %q:\n%s", want, body)
		}
	}
	// A threshold presented as current, and stale, is worse than one that
	// admits its age: somebody would plan a payment around it.
	if !strings.Contains(body, "as of ") {
		t.Errorf("listing does not date the threshold it reports:\n%s", body)
	}
	if !strings.Contains(body, time.Now().UTC().Format("2 Jan")) {
		t.Errorf("listing does not carry a readable date:\n%s", body)
	}
}

// grantRole gives a member a role directly, for the commands that need more
// than the workspace's first admin.
func grantRole(t *testing.T, h *harness, org store.Org, userID string, role chat.Role) store.MemberID {
	t.Helper()
	ctx := context.Background()
	member, err := h.store.EnsureMember(ctx, org.ID, chat.Telegram, userID, "bo")
	if err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	admin, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "42")
	if err != nil || !ok {
		t.Fatalf("admin: %v (found %v)", err, ok)
	}
	if err := h.store.GrantRole(ctx, org.ID, member.ID, role, admin.ID); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	return member.ID
}

// treasuryFor links a treasury directly, skipping the SEP-10 handshake the api
// tests already cover.
func treasuryFor(t *testing.T, h *harness, org store.Org) store.Treasury {
	t.Helper()
	tr, err := h.store.LinkTreasury(context.Background(), store.LinkTreasuryParams{
		Org: org.ID, Kind: store.TreasuryClassic,
		Address: keypair.MustRandom().Address(), Label: "main",
		Low: 2, Medium: 2, High: 2,
	})
	if err != nil {
		t.Fatalf("link treasury: %v", err)
	}
	return tr
}

// orgOf and treasuryOf look up what a test just created in a space.
func (h *harness) orgOf(t *testing.T, space string) store.Org {
	t.Helper()
	org, ok, err := h.store.OrgForSpace(context.Background(), chat.Telegram, space)
	if err != nil || !ok {
		t.Fatalf("org for %s: %v (found %v)", space, err, ok)
	}
	return org
}

func (h *harness) treasuryOf(t *testing.T, space string) store.Treasury {
	t.Helper()
	list, err := h.store.Treasuries(context.Background(), h.orgOf(t, space).ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("treasuries for %s: %v (%d found)", space, err, len(list))
	}
	return list[0]
}
