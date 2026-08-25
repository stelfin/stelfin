package api

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newEnrollTokens(t *testing.T) *EnrollTokens {
	t.Helper()
	c, err := NewEnrollTokens(testSecret)
	if err != nil {
		t.Fatalf("NewEnrollTokens: %v", err)
	}
	return c
}

func TestEnrollTokenRoundTrip(t *testing.T) {
	c := newEnrollTokens(t)

	token, err := c.Issue(scope("+2348012345678"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if want := scope("+2348012345678"); got != want {
		t.Errorf("verified %+v, want %+v", got, want)
	}
}

// TestEnrollTokenCarriesTheOrg: with several tenants, a token naming an owner
// alone would authorise whichever org's row happened to be found.
func TestEnrollTokenCarriesTheOrg(t *testing.T) {
	c := newEnrollTokens(t)

	token, err := c.Issue(Scope{Org: 42, OwnerRef: "telegram:9"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Org != 42 {
		t.Errorf("org = %d, want 42", got.Org)
	}
}

// TestEnrollTokenRefusesAnOrglessScope: a scope with no org names nothing, and
// minting authority for it would produce a token nothing can safely honour.
func TestEnrollTokenRefusesAnOrglessScope(t *testing.T) {
	c := newEnrollTokens(t)
	if _, err := c.Issue(Scope{OwnerRef: "alice"}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("issued a token with no org")
	}
}

// TestEnrollTokenRejectsTampering mirrors TestConfirmTokenRejectsTampering:
// a leaked enroll link must not be editable into authority over someone else's
// account, or over another org's.
func TestEnrollTokenRejectsTampering(t *testing.T) {
	c := newEnrollTokens(t)
	token, err := c.Issue(scope("alice"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	forger, err := NewEnrollTokens([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewEnrollTokens: %v", err)
	}
	forged, err := forger.Issue(scope("mallory"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue forged: %v", err)
	}

	for name, bad := range map[string]string{
		"payload swapped":     enrollTokenVersion + "." + strings.Split(forged, ".")[1] + "." + parts[2],
		"signature stripped":  parts[0] + "." + parts[1],
		"signature mutated":   parts[0] + "." + parts[1] + "." + flipLast(parts[2]),
		"version changed":     "e1." + parts[1] + "." + parts[2],
		"forged with own key": forged,
		"empty":               "",
		"garbage":             "not-a-token",
	} {
		if _, err := c.Verify(bad); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

// TestEnrollTokenIsNotAConfirmToken: even sharing a secret, an enroll token
// must not verify as a confirm token or vice versa — this is the whole reason
// the version is signed rather than just prefixed.
func TestEnrollTokenIsNotAConfirmToken(t *testing.T) {
	enrollTokens := newEnrollTokens(t)
	confirmTokens := newTokens(t)

	enrollTok, err := enrollTokens.Issue(scope("alice"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue enroll token: %v", err)
	}
	if _, _, err := confirmTokens.Verify(enrollTok); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("confirm tokens accepted an enroll token: error = %v, want ErrTokenInvalid", err)
	}

	confirmTok, err := confirmTokens.Issue(scope("alice"), strings.Repeat("a", 64), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue confirm token: %v", err)
	}
	if _, err := enrollTokens.Verify(confirmTok); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("enroll tokens accepted a confirm token: error = %v, want ErrTokenInvalid", err)
	}
}

func TestEnrollTokenExpires(t *testing.T) {
	c := newEnrollTokens(t)
	token, err := c.Issue(scope("alice"), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := c.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("error = %v, want ErrTokenExpired", err)
	}
}

func TestEnrollTokenRequiresAStrongSecret(t *testing.T) {
	if _, err := NewEnrollTokens([]byte("too short")); err == nil {
		t.Fatal("expected an error for a short secret")
	}
}

func TestEnrollTokenRejectsNULInOwner(t *testing.T) {
	c := newEnrollTokens(t)
	if _, err := c.Issue(Scope{Org: 1, OwnerRef: "alice\x00mallory"}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected an error for a NUL in the owner")
	}
}
