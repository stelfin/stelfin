package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAnMCPTokenIsShownOnceAndStoredAsAHash(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "mcp-issue")

	token, record, err := s.IssueMCPToken(ctx, tn.org.ID, "grafana", TierRead, identityOf(t, tn), nil)
	must(t, err, "issue token")

	if !strings.HasPrefix(token, "stlf_") {
		t.Errorf("the token is not recognisable as ours: %q", token)
	}
	if record.Tier != TierRead || record.Label != "grafana" {
		t.Fatalf("record = %+v", record)
	}

	// The plaintext is nowhere in the database. A dump gives a list of what
	// exists rather than a set of working keys.
	var found int
	must(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_tokens WHERE token_hash = $1`, token).Scan(&found),
		"search for the plaintext")
	if found != 0 {
		t.Fatal("the token is stored in the clear")
	}

	got, err := s.LookupMCPToken(ctx, token)
	must(t, err, "look up token")
	if got.ID != record.ID || got.Org != tn.org.ID {
		t.Fatalf("looked up %+v", got)
	}
	// Use is recorded, so a workspace can see which tokens are live before
	// revoking one.
	if got.LastUsedAt == nil {
		t.Error("use was not recorded")
	}
}

func TestARevokedTokenStopsWorkingAndStaysOnTheRecord(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "mcp-revoke")

	token, record, err := s.IssueMCPToken(ctx, tn.org.ID, "ci", TierRead, 0, nil)
	must(t, err, "issue token")

	must(t, s.RevokeMCPToken(ctx, tn.org.ID, record.ID), "revoke")
	if _, err := s.LookupMCPToken(ctx, token); !errors.Is(err, ErrNoToken) {
		t.Fatalf("a revoked token still works: %v", err)
	}

	// The row survives: "revoked" and "never existed" are different answers to
	// somebody asking why an integration stopped.
	list, err := s.MCPTokens(ctx, tn.org.ID)
	must(t, err, "list tokens")
	if len(list) != 1 || list[0].RevokedAt == nil {
		t.Fatalf("tokens = %+v", list)
	}
	if list[0].Live(time.Now()) {
		t.Error("a revoked token reports itself live")
	}

	// Revoking twice is refused rather than silently re-stamping the time.
	if err := s.RevokeMCPToken(ctx, tn.org.ID, record.ID); !errors.Is(err, ErrNoToken) {
		t.Errorf("second revocation: %v", err)
	}
}

func TestAnExpiredTokenStopsWorking(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "mcp-expiry")

	past := time.Now().Add(-time.Minute)
	// The database refuses an expiry before creation outright.
	if _, _, err := s.IssueMCPToken(ctx, tn.org.ID, "stale", TierRead, 0, &past); err == nil {
		t.Fatal("a token expiring before it was created was issued")
	}

	soon := time.Now().Add(50 * time.Millisecond)
	token, _, err := s.IssueMCPToken(ctx, tn.org.ID, "brief", TierRead, 0, &soon)
	must(t, err, "issue token")

	if _, err := s.LookupMCPToken(ctx, token); err != nil {
		t.Fatalf("a live token was refused: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := s.LookupMCPToken(ctx, token); !errors.Is(err, ErrNoToken) {
		t.Fatalf("an expired token still works: %v", err)
	}
}

// TestATokenIsScopedToItsOrg: the token carries the org, so a workspace cannot
// revoke or enumerate another's.
func TestATokenIsScopedToItsOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "mcp-scope-a")
	b := newTenant(t, s, "mcp-scope-b")

	token, record, err := s.IssueMCPToken(ctx, a.org.ID, "theirs", TierRead, 0, nil)
	must(t, err, "issue token")

	if err := s.RevokeMCPToken(ctx, b.org.ID, record.ID); !errors.Is(err, ErrNoToken) {
		t.Errorf("another org revoked it: %v", err)
	}
	if list, err := s.MCPTokens(ctx, b.org.ID); err != nil || len(list) != 0 {
		t.Errorf("another org listed it: %d, %v", len(list), err)
	}
	// And it still works for the org that holds it.
	got, err := s.LookupMCPToken(ctx, token)
	must(t, err, "look up token")
	if got.Org != a.org.ID {
		t.Errorf("the token resolved to org %d", got.Org)
	}
}

func TestAnUnknownTokenIsRefusedWithoutSayingWhy(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	for _, bad := range []string{
		"",
		"not-ours",
		"stlf_" + strings.Repeat("A", 43),
		"Bearer stlf_x",
	} {
		if _, err := s.LookupMCPToken(ctx, bad); !errors.Is(err, ErrNoToken) {
			t.Errorf("%q: error = %v, want ErrNoToken", bad, err)
		}
	}
}

func TestATokenNeedsALabelAndAKnownTier(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "mcp-validate")

	if _, _, err := s.IssueMCPToken(ctx, tn.org.ID, "  ", TierRead, 0, nil); err == nil {
		t.Error("a token with no label was issued")
	}
	// A third tier has to be a migration, not a string somebody passes.
	if _, _, err := s.IssueMCPToken(ctx, tn.org.ID, "x", "sign", 0, nil); err == nil {
		t.Error("an unknown tier was accepted")
	}
}

// TestTwoTokensAreDifferent: 32 bytes of randomness each, so a workspace's
// second token is not a variation on its first.
func TestTwoTokensAreDifferent(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "mcp-unique")

	first, _, err := s.IssueMCPToken(ctx, tn.org.ID, "one", TierRead, 0, nil)
	must(t, err, "issue first")
	second, _, err := s.IssueMCPToken(ctx, tn.org.ID, "two", TierPropose, 0, nil)
	must(t, err, "issue second")

	if first == second {
		t.Fatal("two tokens are identical")
	}
	if len(first) < 40 {
		t.Errorf("a token is only %d characters", len(first))
	}
}
