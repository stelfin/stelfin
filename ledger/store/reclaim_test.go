package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/internal/money"
)

func reclaimFor(t *testing.T, s *Store, tn *tenant, hash, address string) Reclaim {
	t.Helper()
	r := Reclaim{
		Hash: hash, Org: tn.org.ID, OwnerRef: "telegram:u" + tn.org.Slug,
		Address: address, Destination: keypair.MustRandom().Address(),
		XDR:       "AAAAAgAAAAA=",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	must(t, s.SaveReclaim(context.Background(), r), "save reclaim")
	return r
}

func TestReclaimRoundTrip(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "rc-round")

	r := reclaimFor(t, s, tn, hashOf(50), keypair.MustRandom().Address())

	got, err := s.LiveReclaim(ctx, tn.org.ID, r.Hash)
	must(t, err, "live reclaim")
	if got.Address != r.Address || got.Destination != r.Destination {
		t.Fatalf("read back %+v", got)
	}
}

// TestReclaimIsClaimedOnce: the account is about to be deleted. A second
// submission cannot double-spend, but it can produce a second confusing failure
// and a second attempt to release the same grant.
func TestReclaimIsClaimedOnce(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "rc-once")

	r := reclaimFor(t, s, tn, hashOf(51), keypair.MustRandom().Address())

	if _, err := s.ClaimReclaim(ctx, tn.org.ID, r.Hash); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.ClaimReclaim(ctx, tn.org.ID, r.Hash); !errors.Is(err, ErrNoReclaim) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := s.LiveReclaim(ctx, tn.org.ID, r.Hash); !errors.Is(err, ErrNoReclaim) {
		t.Fatalf("a submitted reclaim is still live: %v", err)
	}
}

// TestReclaimSupersedes: two envelopes sweeping the same balance to the same
// place can only ever have one land, and the rest fail later for a reason
// nobody would connect to this.
func TestReclaimSupersedes(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "rc-supersede")
	address := keypair.MustRandom().Address()

	first := reclaimFor(t, s, tn, hashOf(52), address)
	second := reclaimFor(t, s, tn, hashOf(53), address)

	if _, err := s.LiveReclaim(ctx, tn.org.ID, first.Hash); !errors.Is(err, ErrNoReclaim) {
		t.Fatalf("the superseded envelope is still live: %v", err)
	}
	if _, err := s.LiveReclaim(ctx, tn.org.ID, second.Hash); err != nil {
		t.Fatalf("the newest envelope is not live: %v", err)
	}
}

func TestReclaimIsScopedToItsOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "rc-scope-a")
	b := newTenant(t, s, "rc-scope-b")

	r := reclaimFor(t, s, a, hashOf(54), keypair.MustRandom().Address())

	if _, err := s.LiveReclaim(ctx, b.org.ID, r.Hash); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("LiveReclaim across orgs: %v", err)
	}
	if _, err := s.ClaimReclaim(ctx, b.org.ID, r.Hash); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("ClaimReclaim across orgs: %v", err)
	}
	// And it survived the attempt.
	if _, err := s.LiveReclaim(ctx, a.org.ID, r.Hash); err != nil {
		t.Errorf("the owning org lost its reclaim: %v", err)
	}
}

// TestReclaimCannotSweepToItself: the network refuses a self-merge, and
// building one would mean the sponsor address was resolved wrongly — which is
// the kind of mistake that should fail at the write, not on chain.
func TestReclaimCannotSweepToItself(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "rc-self")
	address := keypair.MustRandom().Address()

	err := s.SaveReclaim(context.Background(), Reclaim{
		Hash: hashOf(55), Org: tn.org.ID, OwnerRef: "telegram:x",
		Address: address, Destination: address,
		XDR: "AAAAAgAAAAA=", ExpiresAt: time.Now().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("a self-merge was accepted")
	}
}

// TestReleaseAddressKeepsTheMember: removing the member instead would erase who
// approved what.
func TestReleaseAddressKeepsTheMember(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "rc-release")

	// Give the member the address the tenant already tracks, and a grant
	// against it, as provisioning would have.
	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID, tn.address, AddressProvisioned),
		"set member address")
	linkTreasury(t, s, tn, 1)
	must(t, s.SetEnrollmentPolicy(ctx, tn.org.ID, money.MustParse("100"), 25), "enable provisioning")
	if _, err := testPool.Exec(ctx, `
		INSERT INTO enrollment_grants (org_id, identity_id, member_id, address, reserve_cost)
		VALUES ($1, $2, $3, $4, $5)`,
		int64(tn.org.ID), int64(identityOf(t, tn)), int64(tn.member.ID),
		tn.address, int64(money.MustParse("1.5"))); err != nil {
		t.Fatalf("record grant: %v", err)
	}

	must(t, s.ReleaseAddress(ctx, tn.org.ID, tn.address), "release address")

	member, err := s.Member(ctx, tn.org.ID, tn.member.ID)
	must(t, err, "read member")
	if member.Address != "" {
		t.Errorf("the member still holds %s", member.Address)
	}
	if member.Status != "active" {
		t.Errorf("the member was removed rather than released: %q", member.Status)
	}

	if _, tracked, err := s.TrackedAddress(ctx, tn.address); err != nil || tracked {
		t.Errorf("the address is still tracked: %v, %v", tracked, err)
	}

	grant, ok, err := s.GrantFor(ctx, tn.org.ID, tn.address)
	must(t, err, "grant")
	if !ok || !grant.Reclaimed {
		t.Errorf("the grant was not marked reclaimed: %+v (found %v)", grant, ok)
	}
}
