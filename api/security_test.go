package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
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

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"entry":[{"changes":[]}]}`)
	if err := VerifySignature(testSecret, sign(testSecret, body), body); err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
}

func TestVerifySignatureRejects(t *testing.T) {
	body := []byte(`{"entry":[]}`)
	valid := sign(testSecret, body)

	cases := map[string]struct {
		header string
		body   []byte
		want   error
	}{
		"missing header":   {"", body, ErrSignatureMissing},
		"wrong secret":     {sign([]byte("ffffffffffffffffffffffffffffffff"), body), body, ErrSignatureInvalid},
		"tampered body":    {valid, []byte(`{"entry":[{"evil":true}]}`), ErrSignatureInvalid},
		"no algorithm":     {strings.TrimPrefix(valid, "sha256="), body, ErrSignatureInvalid},
		"downgraded algo":  {"sha1=" + strings.TrimPrefix(valid, "sha256="), body, ErrSignatureInvalid},
		"not hex":          {"sha256=zzzz", body, ErrSignatureInvalid},
		"empty digest":     {"sha256=", body, ErrSignatureInvalid},
		"truncated digest": {valid[:len(valid)-2], body, ErrSignatureInvalid},
	}
	for name, c := range cases {
		if err := VerifySignature(testSecret, c.header, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %v", name, err, c.want)
		}
	}
}

// TestVerifySignatureCoversRawBytes pins the mistake that breaks most webhook
// implementations: verifying re-serialized JSON instead of the bytes received.
// These two bodies are the same object and different bytes.
func TestVerifySignatureCoversRawBytes(t *testing.T) {
	received := []byte(`{"b":1,"a":2}`)
	reserialized := []byte(`{"a":2,"b":1}`)

	header := sign(testSecret, received)
	if err := VerifySignature(testSecret, header, received); err != nil {
		t.Fatalf("raw bytes should verify: %v", err)
	}
	if err := VerifySignature(testSecret, header, reserialized); !errors.Is(err, ErrSignatureInvalid) {
		t.Error("re-serialized JSON verified against the raw-body signature; " +
			"a implementation that parses before verifying would appear to work and would not be checking anything")
	}
}

func TestReadVerifiedBody(t *testing.T) {
	body := []byte(`{"entry":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))

	got, err := ReadVerifiedBody(testSecret, req)
	if err != nil {
		t.Fatalf("ReadVerifiedBody: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestReadVerifiedBodyBoundsSize: the endpoint is unauthenticated until the
// signature is checked, so an unbounded read is a way to exhaust memory
// without ever presenting a credential.
func TestReadVerifiedBodyBoundsSize(t *testing.T) {
	huge := bytes.Repeat([]byte("a"), int(MaxWebhookBody)+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(huge))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, huge))

	if _, err := ReadVerifiedBody(testSecret, req); err == nil {
		t.Fatal("expected an oversized body to be refused")
	}
}

func TestVerifyChallenge(t *testing.T) {
	query := map[string][]string{
		"hub.mode":         {"subscribe"},
		"hub.verify_token": {"correct-token"},
		"hub.challenge":    {"1158201444"},
	}
	got, err := VerifyChallenge("correct-token", query)
	if err != nil {
		t.Fatalf("VerifyChallenge: %v", err)
	}
	if got != "1158201444" {
		t.Errorf("challenge = %q, want %q", got, "1158201444")
	}
}

func TestVerifyChallengeRejects(t *testing.T) {
	base := func() map[string][]string {
		return map[string][]string{
			"hub.mode":         {"subscribe"},
			"hub.verify_token": {"correct-token"},
			"hub.challenge":    {"123"},
		}
	}

	for name, mutate := range map[string]func(map[string][]string){
		"wrong token":   func(q map[string][]string) { q["hub.verify_token"] = []string{"wrong"} },
		"missing token": func(q map[string][]string) { delete(q, "hub.verify_token") },
		"wrong mode":    func(q map[string][]string) { q["hub.mode"] = []string{"unsubscribe"} },
		"no challenge":  func(q map[string][]string) { delete(q, "hub.challenge") },
	} {
		q := base()
		mutate(q)
		if _, err := VerifyChallenge("correct-token", q); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

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
