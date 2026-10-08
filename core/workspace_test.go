package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// discordMessage is a slash command arriving from a Discord server.
func discordMessage(guild, userID, command string, options map[string]string) chat.Inbound {
	m := message(guild, command, "")
	m.DedupeID = strings.Replace(m.DedupeID, "telegram:", "discord:", 1)
	m.Actor = chat.Actor{Channel: chat.Discord, UserID: userID, Handle: "ada#1"}
	m.Conversation = chat.Conversation{Channel: chat.Discord, SpaceID: guild}
	m.Options = options
	return m
}

var spaceCode = regexp.MustCompile(`/connect ([A-Z2-9]{16})`)

// TestASecondSpaceJoinsTheSameWorkspace: a DAO with a Telegram group and a
// Discord server is one tenant with one treasury. The second space gets in
// with a code only an admin of the workspace can ask for, presented by someone
// who administers the space being added — each side proving the half it can.
func TestASecondSpaceJoinsTheSameWorkspace(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460001")
	treasuryFor(t, h, org)

	if err := h.handle(t, message("-460001", "connect", "")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	found := spaceCode.FindStringSubmatch(h.out.only(t).Text)
	if found == nil {
		t.Fatalf("no code in:\n%s", h.out.only(t).Text)
	}
	code := found[1]

	// The Discord server knows nothing yet, and says nothing to anyone.
	h.out = &fakeReplier{}
	if err := h.handle(t, discordMessage("guild-460001", "777", "treasuries", nil)); err != nil {
		t.Fatalf("treasuries before connecting: %v", err)
	}
	if n := len(h.out.sent); n != 0 {
		t.Fatalf("an unconnected space got %d replies", n)
	}

	if err := h.handle(t, discordMessage("guild-460001", "777", "connect", map[string]string{"code": strings.ToLower(code)})); err != nil {
		t.Fatalf("connect with code: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "Acme") {
		t.Fatalf("reply:\n%s", body)
	}

	joined, ok, err := h.store.OrgForSpace(context.Background(), chat.Discord, "guild-460001")
	if err != nil || !ok || joined.ID != org.ID {
		t.Fatalf("the Discord server resolves to org %d (found %v, err %v), want %d", joined.ID, ok, err, org.ID)
	}

	// Being let into the room is not being given a role in it.
	h.out = &fakeReplier{}
	if err := h.handle(t, discordMessage("guild-460001", "777", "treasuries", nil)); err != nil {
		t.Fatalf("treasuries: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the observer role") {
		t.Fatalf("whoever connected the space was handed a role with it:\n%s", body)
	}

	// The code is spent.
	h.out = &fakeReplier{}
	if err := h.handle(t, discordMessage("guild-460002", "778", "connect", map[string]string{"code": code})); err != nil {
		t.Fatalf("second use: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "isn't valid") {
		t.Fatalf("a spent code attached a second space:\n%s", body)
	}
}

// Whoever can add the bot to a room must not be able to attach that room to a
// workspace on the strength of a code alone, and a member of a workspace who is
// not its admin must not be able to mint one.
func TestConnectingNeedsStandingOnBothSides(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460003")
	grantRole(t, h, org, "99", chat.RoleApprover)

	if err := h.handle(t, messageFrom("99", "-460003", "connect", "")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the admin role") {
		t.Fatalf("a non-admin was given a code:\n%s", body)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460003", "connect", "")); err != nil {
		t.Fatalf("connect: %v", err)
	}
	code := spaceCode.FindStringSubmatch(h.out.only(t).Text)[1]

	// Somebody with the code, in a space they do not administer.
	outsider := newHarness(t, false)
	if err := outsider.handle(t, discordMessage("guild-460003", "777", "connect", map[string]string{"code": code})); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if body := outsider.out.only(t).Text; !strings.Contains(body, "Only an administrator") {
		t.Fatalf("a non-administrator attached a space:\n%s", body)
	}
	if _, ok, _ := h.store.OrgForSpace(context.Background(), chat.Discord, "guild-460003"); ok {
		t.Fatal("the space was attached")
	}
}

// A saved name is private to whoever saved it: "send 50 to mum" has to mean the
// speaker's mother, and never somebody else's.
func TestSavedRecipientsBelongToWhoeverSavedThem(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460004")
	mum, other := keypair.MustRandom().Address(), keypair.MustRandom().Address()

	if err := h.handle(t, message("-460004", "save", "my mum "+mum)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, `"my mum"`) {
		t.Fatalf("a name with a space in it was not kept whole:\n%s", body)
	}

	// The same name again is refused rather than repointed.
	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460004", "save", "My Mum "+other)); err != nil {
		t.Fatalf("save again: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "already have") {
		t.Fatalf("a saved name was silently repointed:\n%s", body)
	}

	// Another member has no "my mum".
	memberOf(t, h, org, "99")
	h.out = &fakeReplier{}
	if err := h.handle(t, messageFrom("99", "-460004", "saved", "")); err != nil {
		t.Fatalf("saved: %v", err)
	}
	if body := h.out.only(t).Text; strings.Contains(body, mum) {
		t.Fatalf("one member's saved recipient was shown to another:\n%s", body)
	}

	for args, want := range map[string]string{
		"mum":              "Try: /save",
		"mum NOTANADDRESS": "isn't a Stellar account",
		mum + " " + other:  "can't itself be an address",
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-460004", "save", args)); err != nil {
			t.Fatalf("save %q: %v", args, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, want) {
			t.Errorf("/save %q: want %q in:\n%s", args, want, body)
		}
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460004", "forget", "MY MUM")); err != nil {
		t.Fatalf("forget: %v", err)
	}
	list, err := h.store.Beneficiaries(context.Background(), org.ID, "telegram:42")
	if err != nil || len(list) != 0 {
		t.Fatalf("after forgetting: %v, %v", list, err)
	}
}

func TestLimitsAreSetByAdminsAndReadByMembers(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460005")
	grantRole(t, h, org, "99", chat.RoleProposer)

	if err := h.handle(t, messageFrom("99", "-460005", "limits", "payment 10")); err != nil {
		t.Fatalf("limits: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the admin role") {
		t.Fatalf("a proposer changed the limits:\n%s", body)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460005", "limits", "daily 20000 payment 5000")); err != nil {
		t.Fatalf("limits: %v", err)
	}
	policy, err := h.store.SpendPolicyOf(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if policy.PerPayment != money.MustParse("5000") || policy.Daily != money.MustParse("20000") {
		t.Fatalf("policy = %+v", policy)
	}

	// Both lifted at once: the two values are the same word, which is exactly
	// the case a map keyed on the value gets wrong.
	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460005", "limits", "payment none daily none")); err != nil {
		t.Fatalf("limits: %v", err)
	}
	if policy, _ = h.store.SpendPolicyOf(context.Background(), org.ID); policy != (store.SpendPolicy{}) {
		t.Fatalf("policy = %+v after lifting both", policy)
	}

	for args, want := range map[string]string{
		"payment":               "Try: /limits",
		"weekly 5":              "Try: /limits",
		"payment lots":          "couldn't read",
		"payment 500 daily 100": "could never be reached",
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-460005", "limits", args)); err != nil {
			t.Fatalf("limits %q: %v", args, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, want) {
			t.Errorf("/limits %q: want %q in:\n%s", args, want, body)
		}
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, messageFrom("99", "-460005", "limits", "")); err != nil {
		t.Fatalf("limits: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "one proposal: none") {
		t.Fatalf("a member could not read the limits:\n%s", body)
	}
}

// The token is shown once and only its hash is kept; what it reaches is this
// workspace and no other.
func TestAnAgentTokenIsMintedListedAndRevoked(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460006")
	ctx := context.Background()

	if err := h.handle(t, message("-460006", "agent-token", "reporting")); err != nil {
		t.Fatalf("agent-token: %v", err)
	}
	body := h.out.only(t).Text
	token := regexp.MustCompile(`stelfin_\S+|sf[a-z]*_\S+`).FindString(body)
	if token == "" {
		// The prefix is the store's business; find the one long opaque word.
		for _, w := range strings.Fields(body) {
			if len(w) > 40 {
				token = w
			}
		}
	}
	record, err := h.store.LookupMCPToken(ctx, token)
	if err != nil || record.Org != org.ID || record.Tier != store.TierRead {
		t.Fatalf("the token in the reply resolves to %+v (%v), want a read token for org %d", record, err, org.ID)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460006", "agent-tokens", "")); err != nil {
		t.Fatalf("agent-tokens: %v", err)
	}
	if listing := h.out.only(t).Text; strings.Contains(listing, token) || !strings.Contains(listing, "reporting") {
		t.Fatalf("the listing should name the token and never show it:\n%s", listing)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460006", "agent-revoke", fmt.Sprintf("#%d", record.ID))); err != nil {
		t.Fatalf("agent-revoke: %v", err)
	}
	if _, err := h.store.LookupMCPToken(ctx, token); err == nil {
		t.Fatal("a revoked token still resolves")
	}
}

// Discord delivers nothing to a bot but slash commands, so /send is the only
// way a sentence gets there. What reaches the decoder is the instruction as it
// was typed, verb included: without "send" there is an amount and a recipient
// and nothing saying what to do with them.
func TestSendHandsOverTheInstructionWithItsVerb(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-460007")

	if err := h.handle(t, message("-460007", "send", "50 to mum")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := h.sender.sentences; len(got) != 1 || got[0] != "send 50 to mum" {
		t.Fatalf("the decoder was given %q", got)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-460007", "send", "")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "Try: /send") {
		t.Fatalf("reply:\n%s", body)
	}
}

// Discord's member picker hands over an account's immutable id, so a role can
// be granted to someone who has never spoken — by id, never by name.
func TestGrantTakesAPickedUser(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460008")
	ctx := context.Background()

	m := message("-460008", "grant", "")
	m.Options = map[string]string{"role": "approver", "user": "31337"}
	if err := h.handle(t, m); err != nil {
		t.Fatalf("grant: %v", err)
	}
	member, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "31337")
	if err != nil || !ok {
		t.Fatalf("picked user: %v (found %v)", err, ok)
	}
	if got := roleOf(t, h, org, member); got != chat.RoleApprover {
		t.Fatalf("role = %s, want approver", got)
	}
}

// A propose token is asked for by name, and what it writes is shown as its
// own wherever a draft is listed.
func TestAProposeTokenAndTheDraftsItWrites(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-460009")
	ctx := context.Background()

	body := said(t, h, message("-460009", "agent-token", "payroll bot propose"))
	if !strings.Contains(body, "write draft payrolls") {
		t.Fatalf("reply:\n%s", body)
	}
	var token string
	for _, w := range strings.Fields(body) {
		if len(w) > 40 {
			token = w
		}
	}
	record, err := h.store.LookupMCPToken(ctx, token)
	if err != nil || record.Tier != store.TierPropose || record.Label != "payroll bot" {
		t.Fatalf("token = %+v (%v), want a propose token labelled \"payroll bot\"", record, err)
	}
	if body := said(t, h, message("-460009", "agent-token", "x")); !strings.Contains(body, "read this workspace") || strings.Contains(body, "draft payrolls") {
		t.Fatalf("a token with no tier was not read-only:\n%s", body)
	}

	if _, err := h.store.SaveDraft(ctx, store.Draft{
		Org: org.ID, AgentToken: record.ID, Total: 50_000_000,
		Rows: []store.DraftRow{{Line: 1, Amount: "5.0000000", Destination: keypair.MustRandom().Address()}},
	}); err != nil {
		t.Fatalf("save agent draft: %v", err)
	}
	if listing := said(t, h, message("-460009", "drafts", "")); !strings.Contains(listing, `by agent "payroll bot"`) {
		t.Fatalf("an agent's draft is not marked as one:\n%s", listing)
	}
}
