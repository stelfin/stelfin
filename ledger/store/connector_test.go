package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAGrantIsInForceOnlyWithSignatures: the guard reads what this returns, so
// "a request is not a grant" has to be true here and not merely in the code
// that calls it — and true of the table itself, so that no later code path can
// make a grant by forgetting to collect signatures.
func TestAGrantIsInForceOnlyWithSignatures(t *testing.T) {
	ctx := context.Background()
	db := New(testPool)
	tn := newTenant(t, db, "-480001")
	org, member := tn.org, tn.member
	identity := identityOf(t, tn)
	treasury := linkTreasury(t, db, tn, 2)
	row, err := db.RegisterConnector(ctx, RegisterConnectorParams{
		Org: org.ID, Name: "payroll", Kind: ConnectorSheets, Ready: true, CreatedBy: member.ID,
	})
	must(t, err, "register connector")

	hash := strings.Repeat("a", 64)
	if err := db.SaveChallenge(ctx, Challenge{
		Hash: hash, Org: org.ID, Identity: identity, Purpose: PurposeGrantConnector,
		Address: treasury.Address, XDR: "AAAA", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save challenge: %v", err)
	}
	if err := db.RequestGrant(ctx, RequestGrantParams{
		Org: org.ID, Connector: row.ID, Treasury: treasury.ID,
		Capabilities: []string{"read.range"}, SurfaceDigest: strings.Repeat("b", 64), PerHour: 60,
		Statement: "s", StatementDigest: strings.Repeat("c", 64), ChallengeHash: hash, RequestedBy: member.ID,
	}); err != nil {
		t.Fatalf("request grant: %v", err)
	}

	if _, err := db.GrantInForce(ctx, org.ID, "payroll"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("a request with no signatures is in force: %v", err)
	}
	if _, err := db.ConsumeGrantChallenge(ctx, org.ID, hash, treasury.Address, "", nil); err == nil {
		t.Fatal("a grant was put in force with no signatures")
	}
	// Not through the store's own checks either: the table refuses.
	if _, err := testPool.Exec(ctx,
		`UPDATE connector_grants SET granted_at = now() WHERE challenge_hash = $1`, hash); err == nil {
		t.Fatal("the table accepted a grant in force with no signed envelope")
	}

	granted, err := db.ConsumeGrantChallenge(ctx, org.ID, hash, treasury.Address, "SIGNED", []string{"GA", "GB"})
	if err != nil {
		t.Fatalf("ConsumeGrantChallenge: %v", err)
	}
	if granted.GrantedAt == nil || len(granted.SignedBy) != 2 {
		t.Fatalf("grant = %+v", granted)
	}
	if _, err := db.ConsumeGrantChallenge(ctx, org.ID, hash, treasury.Address, "SIGNED", []string{"GA"}); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a challenge was spent twice: %v", err)
	}

	// Another workspace asking by the same name finds nothing.
	other := newTenant(t, db, "-480002")
	if _, err := db.GrantInForce(ctx, other.org.ID, "payroll"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("another workspace found this grant: %v", err)
	}

	// Removing the connector ends its grant, and frees the name.
	if err := db.RemoveConnector(ctx, org.ID, "payroll", member.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := db.GrantInForce(ctx, org.ID, "payroll"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("a removed connector still has a grant: %v", err)
	}
	again, err := db.RegisterConnector(ctx, RegisterConnectorParams{
		Org: org.ID, Name: "payroll", Kind: ConnectorSheets, Ready: true, CreatedBy: member.ID,
	})
	if err != nil {
		t.Fatalf("register again: %v", err)
	}
	// The new one is a different connector that happens to share a name. The
	// old one's signatures were not for it.
	if again.ID == row.ID {
		t.Fatal("re-registering reused the removed connector's row")
	}
	if _, err := db.GrantInForce(ctx, org.ID, "payroll"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("a re-registered name inherited the old grant: %v", err)
	}
}
