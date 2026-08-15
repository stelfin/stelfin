package api

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newTokens(t *testing.T) *ConfirmTokens {
	t.Helper()
	c, err := NewConfirmTokens(testSecret)
	if err != nil {
		t.Fatalf("NewConfirmTokens: %v", err)
	}
	return c
}

func TestConfirmTokenRoundTrip(t *testing.T) {
	c := newTokens(t)
	hash := strings.Repeat("a", 64)

	token, err := c.Issue("+2348012345678", hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	owner, gotHash, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if owner != "+2348012345678" || gotHash != hash {
		t.Errorf("verified %q/%q, want %q/%q", owner, gotHash, "+2348012345678", hash)
	}
}

// TestConfirmTokenRejectsTampering is the property the whole scheme rests on:
// a link that reaches a stranger must not be editable into authority over a
// different payment or a different user.
func TestConfirmTokenRejectsTampering(t *testing.T) {
	c := newTokens(t)
	token, err := c.Issue("alice", strings.Repeat("a", 64), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	// Re-signing a forged payload with a different secret must not pass.
	forger, err := NewConfirmTokens([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewConfirmTokens: %v", err)
	}
	forged, err := forger.Issue("mallory", strings.Repeat("b", 64), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue forged: %v", err)
	}

	for name, bad := range map[string]string{
		"payload swapped":     "v1." + strings.Split(forged, ".")[1] + "." + parts[2],
		"signature stripped":  parts[0] + "." + parts[1],
		"signature blanked":   parts[0] + "." + parts[1] + ".",
		"signature mutated":   parts[0] + "." + parts[1] + "." + flipLast(parts[2]),
		"version changed":     "v2." + parts[1] + "." + parts[2],
		"forged with own key": forged,
		"empty":               "",
		"garbage":             "not-a-token",
	} {
		if _, _, err := c.Verify(bad); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

func TestConfirmTokenExpires(t *testing.T) {
	c := newTokens(t)
	token, err := c.Issue("alice", strings.Repeat("a", 64), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, _, err := c.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("error = %v, want ErrTokenExpired", err)
	}
}

// TestConfirmTokenScopesToOneTransaction: a token names one hash, so a leaked
// link cannot authorise a different payment.
func TestConfirmTokenScopesToOneTransaction(t *testing.T) {
	c := newTokens(t)
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)

	token, err := c.Issue("alice", first, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, gotHash, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotHash == second {
		t.Fatal("token verified against a transaction it does not name")
	}
	if gotHash != first {
		t.Errorf("token names %q, want %q", gotHash, first)
	}
}

func TestConfirmTokenRequiresAStrongSecret(t *testing.T) {
	if _, err := NewConfirmTokens([]byte("too short")); err == nil {
		t.Fatal("expected an error for a short secret")
	}
}

func TestConfirmTokenRejectsNULInFields(t *testing.T) {
	c := newTokens(t)
	// The payload is NUL-joined, so a NUL inside a field could re-split into a
	// different owner/hash pair.
	if _, err := c.Issue("alice\x00mallory", strings.Repeat("a", 64), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected an error for a NUL in the owner")
	}
}

// flipLast changes the last character of a token component, so a test can
// present something that is the right shape and the wrong value.
func flipLast(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	if last == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}
