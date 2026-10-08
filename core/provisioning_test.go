package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

func policyOf(t *testing.T, h *harness, org store.Org) store.EnrollmentPolicy {
	t.Helper()
	p, err := h.store.EnrollmentPolicyOf(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	return p
}

// TestProvisioningNeedsATreasuryFirst: turning it on spends the operator's
// money, and the only thing separating a real workspace from a group somebody
// created to drain the float is a treasury proved by signature.
func TestProvisioningNeedsATreasuryFirst(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-450001")

	if p := policyOf(t, h, org); p.On() {
		t.Fatal("a new workspace has provisioning on")
	}

	if err := h.handle(t, message("-450001", "provisioning", "on")); err != nil {
		t.Fatalf("provisioning on: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/link-treasury") {
		t.Fatalf("want to be sent to link a treasury:\n%s", body)
	}
	if p := policyOf(t, h, org); p.On() || p.Budget != 0 {
		t.Fatalf("provisioning was recorded as on with no treasury: %+v", p)
	}

	treasuryFor(t, h, org)
	h.out = &fakeReplier{}
	if err := h.handle(t, message("-450001", "provisioning", "on 10")); err != nil {
		t.Fatalf("provisioning on 10: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "10 wallets a day") {
		t.Fatalf("reply:\n%s", body)
	}
	if p := policyOf(t, h, org); !p.On() || p.DailyCap != 10 {
		t.Fatalf("policy = %+v, want on at 10 a day", p)
	}

	h.out = &fakeReplier{}
	if err := h.handle(t, message("-450001", "provisioning", "off")); err != nil {
		t.Fatalf("provisioning off: %v", err)
	}
	if p := policyOf(t, h, org); p.On() {
		t.Fatalf("policy = %+v after turning it off", p)
	}
}

func TestProvisioningIsForAdminsAndReadsItsArguments(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-450002")
	treasuryFor(t, h, org)
	grantRole(t, h, org, "99", chat.RoleApprover)

	if err := h.handle(t, messageFrom("99", "-450002", "provisioning", "on")); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "needs the admin role") {
		t.Fatalf("a non-admin was not refused:\n%s", body)
	}

	for args, want := range map[string]string{
		"":         "Provisioning is off",
		"on lots":  "couldn't read",
		"on 0":     "couldn't read",
		"on 99999": "couldn't read",
		"maybe":    "Try: /provisioning",
	} {
		h.out = &fakeReplier{}
		if err := h.handle(t, message("-450002", "provisioning", args)); err != nil {
			t.Fatalf("provisioning %q: %v", args, err)
		}
		if body := h.out.only(t).Text; !strings.Contains(body, want) {
			t.Errorf("/provisioning %q: want %q in:\n%s", args, want, body)
		}
		if p := policyOf(t, h, org); p.On() {
			t.Fatalf("/provisioning %q turned it on", args)
		}
	}
}
