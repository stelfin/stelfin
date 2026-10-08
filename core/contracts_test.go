package core

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/settlement"
)

func contractAddress() string {
	raw := make([]byte, 32)
	copy(raw, []byte("some contract or other, testing"))
	address, _ := strkey.Encode(strkey.VersionByteContract, raw)
	return address
}

// Allowing a contract is an admin's; calling one is a proposal, which the
// signers sign. Each refusal says why and what to do about it.
func TestContractsAreAllowedByAdminsAndCalledByProposal(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-490001")
	treasuryFor(t, h, org)
	grantRole(t, h, org, "99", chat.RoleProposer)
	h.sender.draftTo = keypair.MustRandom().Address()
	c := contractAddress()

	if body := said(t, h, messageFrom("99", "-490001", "contract", "allow "+c+" vault")); !strings.Contains(body, "needs the admin role") {
		t.Fatalf("a proposer allowed a contract:\n%s", body)
	}
	body := said(t, h, message("-490001", "contract", "allow "+c+" vault"))
	if !strings.Contains(body, "authorises no call") || !strings.Contains(body, "allowed again") {
		t.Fatalf("allow reply:\n%s", body)
	}
	if listing := said(t, h, messageFrom("99", "-490001", "contracts", "")); !strings.Contains(listing, "vault — "+c) {
		t.Fatalf("listing:\n%s", listing)
	}

	body = said(t, h, messageFrom("99", "-490001", "invoke", "vault withdraw address:"+h.sender.draftTo+" i128:5"))
	if !strings.Contains(body, "/approve#") || !strings.Contains(body, "limits on payments here don't apply") {
		t.Fatalf("invoke reply:\n%s", body)
	}
	if got := h.sender.invoked; len(got) != 1 || got[0] != "vault.withdraw address:"+h.sender.draftTo+" i128:5" {
		t.Fatalf("invoked %q", got)
	}

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"off":     {api.ErrContractCallsOff, "isn't enabled on this deployment"},
		"changed": {api.ErrContractChanged, "its code has been replaced"},
		"args":    {api.ErrBadArgument, "type:value"},
	} {
		h.sender.contractErr = tc.err
		if body := said(t, h, messageFrom("99", "-490001", "invoke", "vault withdraw i128:5")); !strings.Contains(body, tc.want) {
			t.Errorf("%s: want %q in:\n%s", name, tc.want, body)
		}
	}
	h.sender.contractErr = nil

	if body := said(t, h, messageFrom("99", "-490001", "invoke", "nothing x")); !strings.Contains(body, "Nothing is allowed here") {
		t.Fatalf("an unknown label:\n%s", body)
	}
	said(t, h, message("-490001", "contract", "remove vault"))
	if body := said(t, h, messageFrom("99", "-490001", "invoke", "vault withdraw")); !strings.Contains(body, "Nothing is allowed here") {
		t.Fatalf("a removed contract was called:\n%s", body)
	}
}

// A call that finds its state archived is not quietly made to restore it: the
// member is told, and offered the restore as a proposal of its own.
func TestArchivedStateIsAProposalOfItsOwn(t *testing.T) {
	h := newHarness(t, true)
	org := setUpWorkspace(t, h, "-490002")
	treasuryFor(t, h, org)
	h.sender.draftTo = keypair.MustRandom().Address()
	said(t, h, message("-490002", "contract", "allow "+contractAddress()+" vault"))

	h.sender.contractErr = settlement.ErrArchivedState
	body := said(t, h, message("-490002", "invoke", "vault withdraw i128:5"))
	if !strings.Contains(body, "/restore vault withdraw i128:5") || !strings.Contains(body, "won't fold it in") {
		t.Fatalf("invoke on archived state:\n%s", body)
	}

	h.sender.contractErr = api.ErrNothingArchived
	if body := said(t, h, message("-490002", "restore", "vault withdraw i128:5")); !strings.Contains(body, "needs nothing restored") {
		t.Fatalf("restore with nothing archived:\n%s", body)
	}
	h.sender.contractErr = nil
	if body := said(t, h, message("-490002", "restore", "vault withdraw i128:5")); !strings.Contains(body, "/approve#") {
		t.Fatalf("restore reply:\n%s", body)
	}
}
