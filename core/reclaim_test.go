package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// provisionedIn gives the speaker an address, as provisioning would have.
func provisionedIn(t *testing.T, h *harness, space string) (store.Org, string) {
	t.Helper()
	ctx := context.Background()
	org := setUpWorkspace(t, h, space)

	member, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "42")
	if err != nil || !ok {
		t.Fatalf("member: %v (found %v)", err, ok)
	}
	address := keypair.MustRandom().Address()
	if err := h.store.SetMemberAddress(ctx, org.ID, member.ID, address,
		store.AddressProvisioned); err != nil {
		t.Fatalf("set member address: %v", err)
	}
	h.out = &fakeReplier{}
	return org, address
}

func TestReclaimHandsBackAndSaysWhatItCosts(t *testing.T) {
	h := newHarness(t, true)
	_, address := provisionedIn(t, h, "-430001")

	if err := h.handle(t, message("-430001", "reclaim", "")); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "/reclaim#") {
		t.Errorf("reply does not carry a signing link:\n%s", body)
	}
	if !strings.Contains(body, address) {
		t.Errorf("reply does not name the account:\n%s", body)
	}
	// The consequence has to be said before the link, not discovered on the
	// page: this deletes the account and there is no undo.
	for _, want := range []string{"deletes the account", "no undo"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not warn that %q:\n%s", want, body)
		}
	}
	if got := h.sender.reclaimed; len(got) != 1 || got[0] != address {
		t.Errorf("prepared %v, want %q", got, address)
	}
}

// TestReclaimLeavesAMembersOwnWalletAlone: their wallet is theirs. Building
// them a transaction that deletes it would be doing something nobody asked for.
func TestReclaimLeavesAMembersOwnWalletAlone(t *testing.T) {
	h := newHarness(t, true)
	provisionedIn(t, h, "-430002")
	h.sender.reclaimErr = fmt.Errorf("%w: never sponsored", api.ErrNothingToReclaim)

	if err := h.handle(t, message("-430002", "reclaim", "")); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "It's yours") {
		t.Errorf("reply does not say the wallet is theirs:\n%s", body)
	}
	if strings.Contains(body, "/reclaim#") {
		t.Fatalf("a wallet stelfin never paid for got a deletion link:\n%s", body)
	}
}

// TestReclaimExplainsANonEmptyAccount: a trustline can only be removed at zero,
// so the only way the merge could "work" would be to give the balance up.
func TestReclaimExplainsANonEmptyAccount(t *testing.T) {
	h := newHarness(t, true)
	provisionedIn(t, h, "-430003")
	h.sender.reclaimErr = fmt.Errorf("%w: it holds 42.5 USDC", api.ErrAccountNotEmpty)

	if err := h.handle(t, message("-430003", "reclaim", "")); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	body := h.out.only(t).Text
	if !strings.Contains(body, "42.5 USDC") {
		t.Errorf("reply does not name what is in the way:\n%s", body)
	}
	if !strings.Contains(body, "Send the balance") {
		t.Errorf("reply does not say how to fix it:\n%s", body)
	}
	if strings.Contains(body, "/reclaim#") {
		t.Fatalf("a non-empty account got a deletion link:\n%s", body)
	}
}

func TestReclaimNeedsAnAccountFirst(t *testing.T) {
	h := newHarness(t, true)
	setUpWorkspace(t, h, "-430004")

	if err := h.handle(t, message("-430004", "reclaim", "")); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "hand back") {
		t.Errorf("reply does not say there is nothing to do:\n%s", body)
	}
	if len(h.sender.reclaimed) != 0 {
		t.Fatal("a member with no address had a hand-back prepared")
	}
}
