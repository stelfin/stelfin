package api

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stelfin/stelfin/ledger/store"
)

func newApproveTokens(t *testing.T) *ApproveTokens {
	t.Helper()
	c, err := NewApproveTokens(testSecret)
	if err != nil {
		t.Fatalf("NewApproveTokens: %v", err)
	}
	return c
}

func TestApproveTokenRoundTrip(t *testing.T) {
	c := newApproveTokens(t)

	token, err := c.Issue(scope("telegram:9"), store.ProposalID(31), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, proposal, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if want := scope("telegram:9"); got != want {
		t.Errorf("verified %+v, want %+v", got, want)
	}
	if proposal != 31 {
		t.Errorf("proposal = %d, want 31", proposal)
	}
}

// TestApproveTokenIsScopedToOneApprover is the design decision worth pinning.
//
// A proposal needs several approvals, and the shortcut is one shared link
// passed around the channel — a bearer credential anyone who scrolled up can
// use, on a page showing what a treasury is about to spend. Each approver's
// token names them, so two approvers hold two different tokens.
func TestApproveTokenIsScopedToOneApprover(t *testing.T) {
	c := newApproveTokens(t)
	hour := time.Now().Add(time.Hour)

	ada, err := c.Issue(Scope{Org: 7, OwnerRef: "telegram:1"}, 31, hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	bo, err := c.Issue(Scope{Org: 7, OwnerRef: "telegram:2"}, 31, hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if ada == bo {
		t.Fatal("two approvers were issued the same token")
	}

	got, _, err := c.Verify(bo)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.OwnerRef != "telegram:2" {
		t.Errorf("owner = %q", got.OwnerRef)
	}
}

func TestApproveTokenRefusesAnEmptyProposal(t *testing.T) {
	c := newApproveTokens(t)
	for _, id := range []store.ProposalID{0, -1} {
		if _, err := c.Issue(scope("telegram:9"), id, time.Now().Add(time.Hour)); err == nil {
			t.Errorf("issued a token for proposal %d", id)
		}
	}
}

func TestApproveTokenRefusesAnOrglessScope(t *testing.T) {
	c := newApproveTokens(t)
	if _, err := c.Issue(Scope{OwnerRef: "telegram:9"}, 31, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("issued a token with no org")
	}
}

func TestApproveTokenExpires(t *testing.T) {
	c := newApproveTokens(t)
	token, err := c.Issue(scope("telegram:9"), 31, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := c.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("error = %v, want ErrTokenExpired", err)
	}
}

// TestApproveTokenRejectsTampering: a leaked approve link must not be editable
// into authority over another org's proposal, or into somebody else's name.
func TestApproveTokenRejectsTampering(t *testing.T) {
	c := newApproveTokens(t)
	token, err := c.Issue(Scope{Org: 7, OwnerRef: "telegram:1"}, 31, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	forger, err := NewApproveTokens([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewApproveTokens: %v", err)
	}
	forged, err := forger.Issue(Scope{Org: 8, OwnerRef: "telegram:2"}, 99, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Someone else's payload under this issuer's signature, and this payload
	// under someone else's: neither verifies.
	for name, bad := range map[string]string{
		"swapped payload":   parts[0] + "." + strings.Split(forged, ".")[1] + "." + parts[2],
		"swapped signature": parts[0] + "." + parts[1] + "." + strings.Split(forged, ".")[2],
		"whole forgery":     forged,
		"wrong version":     "a2." + parts[1] + "." + parts[2],
		"truncated":         parts[0] + "." + parts[1],
	} {
		if _, _, err := c.Verify(bad); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

// TestApproveTokenIsNotInterchangeableWithTheOthers: four near-identical types
// sharing one secret, and the version is signed into the MAC so none can be
// presented as another. An approve token must not become authority to submit a
// payment, and a confirm token must not become authority to approve one.
func TestApproveTokenIsNotInterchangeableWithTheOthers(t *testing.T) {
	confirm := newTokens(t)
	link := newLinkTokensFor(t)
	enroll := newEnrollTokens(t)
	approve := newApproveTokens(t)

	s := Scope{Org: 7, OwnerRef: "telegram:1"}
	hash := strings.Repeat("a", 64)
	hour := time.Now().Add(time.Hour)

	approveTok, err := approve.Issue(s, 31, hour)
	if err != nil {
		t.Fatalf("issue approve: %v", err)
	}
	confirmTok, err := confirm.Issue(s, hash, hour)
	if err != nil {
		t.Fatalf("issue confirm: %v", err)
	}
	linkTok, err := link.Issue(s, hash, hour)
	if err != nil {
		t.Fatalf("issue link: %v", err)
	}
	enrollTok, err := enroll.Issue(s, hour)
	if err != nil {
		t.Fatalf("issue enroll: %v", err)
	}

	for name, check := range map[string]func() error{
		"approve as confirm": func() error { _, _, err := confirm.Verify(approveTok); return err },
		"approve as link":    func() error { _, _, err := link.Verify(approveTok); return err },
		"approve as enroll":  func() error { _, err := enroll.Verify(approveTok); return err },
		"confirm as approve": func() error { _, _, err := approve.Verify(confirmTok); return err },
		"link as approve":    func() error { _, _, err := approve.Verify(linkTok); return err },
		"enroll as approve":  func() error { _, _, err := approve.Verify(enrollTok); return err },
	} {
		if err := check(); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

// TestApproveTokenFieldsCannotBeResplit: the payload is joined with NUL, which
// cannot occur in an owner reference, so no combination of values can be
// re-split into a different scope.
func TestApproveTokenFieldsCannotBeResplit(t *testing.T) {
	c := newApproveTokens(t)

	token, err := c.Issue(Scope{Org: 7, OwnerRef: "telegram:1"}, 31, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := strings.Count(string(payload), "\x00"); n != 3 {
		t.Fatalf("payload has %d separators, want 3", n)
	}
}
