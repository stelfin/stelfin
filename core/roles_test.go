package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// memberOf returns the member a Telegram user speaks for, having them speak
// first if they never have.
func memberOf(t *testing.T, h *harness, org store.Org, userID string) store.Member {
	t.Helper()
	m, err := h.store.EnsureMember(context.Background(), org.ID, chat.Telegram, userID, "bo")
	if err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	return m
}

func roleOf(t *testing.T, h *harness, org store.Org, m store.Member) chat.Role {
	t.Helper()
	role, err := h.store.EffectiveRole(context.Background(), org.ID, m.ID, chat.Telegram, nil)
	if err != nil {
		t.Fatalf("effective role: %v", err)
	}
	return role
}

// TestGrantIsWhatLetsASecondPersonIn: before /grant existed the installer was
// the only member who could ever hold a role, so a second signer could not be
// offered a link and a multisig treasury could not collect its signatures here.
func TestGrantIsWhatLetsASecondPersonIn(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440001")
	treasuryFor(t, h, org)
	bo := memberOf(t, h, org, "99")

	if err := h.handle(t, messageFrom("99", "-440001", "proposals", "")); err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the observer role") {
		t.Fatalf("a member with no role was served:\n%s", body)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-440001", "grant", fmt.Sprintf("#%d approver", bo.ID))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "an approver") {
		t.Fatalf("grant reply:\n%s", body)
	}
	if got := roleOf(t, h, org, bo); got != chat.RoleApprover {
		t.Fatalf("role = %s, want approver", got)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, messageFrom("99", "-440001", "proposals", "")); err != nil {
		t.Fatalf("proposals: %v", err)
	}
	if body := h.out.only(t).Text; strings.Contains(body, "needs the") {
		t.Fatalf("an approver was still refused:\n%s", body)
	}
}

// A grant replaces the role held. Adding to it would make a demotion a no-op,
// because the effective role is the highest one held.
func TestGrantReplacesAndRevokeRemoves(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440002")
	bo := memberOf(t, h, org, "99")

	for _, step := range []struct {
		command, args string
		want          chat.Role
	}{
		{"grant", fmt.Sprintf("%d approver", bo.ID), chat.RoleApprover},
		{"grant", fmt.Sprintf("%d observer", bo.ID), chat.RoleObserver},
		{"revoke", fmt.Sprintf("%d", bo.ID), chat.RoleNone},
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-440002", step.command, step.args)); err != nil {
			t.Fatalf("%s %s: %v", step.command, step.args, err)
		}
		if got := roleOf(t, h, org, bo); got != step.want {
			t.Fatalf("after /%s %s: role = %s, want %s", step.command, step.args, got, step.want)
		}
	}
}

func TestGrantIsForAdminsOnly(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440003")
	bo := memberOf(t, h, org, "99")
	grantRole(t, h, org, "99", chat.RoleApprover)

	// An approver promoting themselves.
	if err := h.handle(t, messageFrom("99", "-440003", "grant", fmt.Sprintf("%d admin", bo.ID))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the admin role") {
		t.Fatalf("a non-admin was not refused:\n%s", body)
	}
	if got := roleOf(t, h, org, bo); got != chat.RoleApprover {
		t.Fatalf("role = %s after a refused grant", got)
	}
}

// A workspace with no admin cannot be repaired from chat, so the last one
// cannot be removed — by revoking, or by granting something lesser.
func TestTheLastAdminCannotBeRemoved(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440004")
	admin, ok, err := h.store.MemberByIdentity(context.Background(), org.ID, chat.Telegram, "42")
	if err != nil || !ok {
		t.Fatalf("admin: %v (found %v)", err, ok)
	}

	for _, m := range []chat.Inbound{
		message("-440004", "revoke", fmt.Sprintf("%d", admin.ID)),
		message("-440004", "grant", fmt.Sprintf("%d observer", admin.ID)),
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, m); err != nil {
			t.Fatalf("/%s: %v", m.Command, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, "no admin") {
			t.Fatalf("/%s %s was not refused:\n%s", m.Command, m.Args, body)
		}
		if got := roleOf(t, h, org, admin); got != chat.RoleAdmin {
			t.Fatalf("the only admin is now %s", got)
		}
	}

	// With a second admin, the first may step down.
	bo := memberOf(t, h, org, "99")
	h.out = &fakeReplier{}
	if err := h.handle(t, message("-440004", "grant", fmt.Sprintf("%d admin", bo.ID))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.out = &fakeReplier{}
	if err := h.handle(t, message("-440004", "revoke", fmt.Sprintf("%d", admin.ID))); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := roleOf(t, h, org, admin); got != chat.RoleNone {
		t.Fatalf("role = %s after stepping down", got)
	}
}

// A member number from another workspace names nobody here. Tenancy has to
// hold for the one command whose whole job is handing out access.
func TestGrantCannotReachAnotherWorkspace(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-440005")
	other := setUpWorkspace(t, h, "-440006")
	outsider := memberOf(t, h, other, "99")

	if err := h.handle(t, message("-440005", "grant", fmt.Sprintf("%d admin", outsider.ID))); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "no member") {
		t.Fatalf("a member of another workspace was granted a role:\n%s", body)
	}
	if got := roleOf(t, h, other, outsider); got != chat.RoleNone {
		t.Fatalf("the outsider now holds %s in their own workspace", got)
	}
}

func TestGrantExplainsWhatItCouldNotRead(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-440007")

	for args, want := range map[string]string{
		"":             "Try: /grant",
		"12":           "Try: /grant",
		"ada admin":    "member number",
		"12 owner":     "don't know a role",
		"999999 admin": "no member",
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-440007", "grant", args)); err != nil {
			t.Fatalf("grant %q: %v", args, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, want) {
			t.Errorf("/grant %q: want %q in:\n%s", args, want, body)
		}
	}
}

func TestMembersListsNumbersAndRoles(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440008")
	bo := memberOf(t, h, org, "99")

	if err := h.handle(t, message("-440008", "members", "")); err != nil {
		t.Fatalf("members: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{fmt.Sprintf("#%d", bo.ID), "admin", "none"} {
		if !strings.Contains(body, want) {
			t.Errorf("want %q in:\n%s", want, body)
		}
	}
}

// On Telegram, "/grant approver" in reply to someone's message arrives as the
// role in the text and the person as a picked id.
func TestGrantInReplyToSomeone(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-440009")

	m := message("-440009", "grant", "approver")
	m.Options = map[string]string{"user": "5150"}
	if err := h.handle(t, m); err != nil {
		t.Fatalf("grant: %v", err)
	}
	member, ok, err := h.store.MemberByIdentity(context.Background(), org.ID, chat.Telegram, "5150")
	if err != nil || !ok {
		t.Fatalf("picked member: %v (found %v)", err, ok)
	}
	if got := roleOf(t, h, org, member); got != chat.RoleApprover {
		t.Fatalf("role = %s, want approver", got)
	}

	h.out = &fakeReplier{}
	m = message("-440009", "revoke", "")
	m.Options = map[string]string{"user": "5150"}
	if err := h.handle(t, m); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := roleOf(t, h, org, member); got != chat.RoleNone {
		t.Fatalf("role = %s after revoking in reply", got)
	}
}
