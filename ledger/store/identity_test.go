package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
)

// identityOf returns the chat identity a tenant's member speaks through.
func identityOf(t *testing.T, tn *tenant) IdentityID {
	t.Helper()
	var id IdentityID
	must(t, testPool.QueryRow(context.Background(),
		`SELECT id FROM user_identities WHERE org_id = $1 AND member_id = $2 LIMIT 1`,
		int64(tn.org.ID), int64(tn.member.ID)).Scan(&id), "look up identity")
	return id
}

// challengeFor builds a saved challenge for a tenant's member.
func challengeFor(t *testing.T, s *Store, tn *tenant, hash, address string) Challenge {
	t.Helper()
	ctx := context.Background()
	identity := identityOf(t, tn)

	c := Challenge{
		Hash: hash, Org: tn.org.ID, Identity: identity,
		Purpose: PurposeLinkMember, Address: address,
		XDR:       "AAAAAgAAAAA=",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	must(t, s.SaveChallenge(ctx, c), "save challenge")
	return c
}

func hashOf(n byte) string {
	out := make([]byte, 64)
	const hex = "0123456789abcdef"
	for i := range out {
		out[i] = hex[(int(n)+i)%16]
	}
	return string(out)
}

func TestChallengeRoundTrip(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300001")
	address := keypair.MustRandom().Address()

	c := challengeFor(t, s, tn, hashOf(1), address)

	got, err := s.LiveChallenge(ctx, tn.org.ID, c.Hash)
	must(t, err, "live challenge")
	if got.Address != address || got.Purpose != PurposeLinkMember {
		t.Fatalf("challenge = %+v", got)
	}

	member, err := s.ConsumeChallenge(ctx, tn.org.ID, c.Hash, address, AddressLinked)
	must(t, err, "consume challenge")
	if member != tn.member.ID {
		t.Errorf("bound to member %d, want %d", member, tn.member.ID)
	}

	bound, err := s.Member(ctx, tn.org.ID, tn.member.ID)
	must(t, err, "read member")
	if bound.Address != address {
		t.Errorf("member address = %q, want %q", bound.Address, address)
	}
	if bound.VerifiedAt.Year() < 2000 {
		t.Error("the address was bound without recording the proof that produced it")
	}
}

// TestChallengeIsSpentOnce: two submissions of the same signed challenge cannot
// both succeed, whatever the interleaving.
func TestChallengeIsSpentOnce(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300002")
	address := keypair.MustRandom().Address()
	c := challengeFor(t, s, tn, hashOf(2), address)

	if _, err := s.ConsumeChallenge(ctx, tn.org.ID, c.Hash, address, AddressLinked); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := s.ConsumeChallenge(ctx, tn.org.ID, c.Hash, address, AddressLinked); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("second consume error = %v, want ErrNoChallenge", err)
	}
	if _, err := s.LiveChallenge(ctx, tn.org.ID, c.Hash); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a spent challenge is still live: %v", err)
	}
}

// TestChallengeCannotBindADifferentAddress: the address is part of the claim.
// Consuming a challenge issued for one account as proof of another would be the
// whole point of the signature, undone.
func TestChallengeCannotBindADifferentAddress(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300003")
	issued := keypair.MustRandom().Address()
	other := keypair.MustRandom().Address()
	c := challengeFor(t, s, tn, hashOf(3), issued)

	if _, err := s.ConsumeChallenge(ctx, tn.org.ID, c.Hash, other, AddressLinked); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("error = %v, want ErrNoChallenge", err)
	}
}

// TestChallengeIsScopedToItsOrg: a hash from another tenant must not resolve.
func TestChallengeIsScopedToItsOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	mine := newTenant(t, s, "-300004")
	theirs := newTenant(t, s, "-300005")
	address := keypair.MustRandom().Address()
	c := challengeFor(t, s, mine, hashOf(4), address)

	if _, err := s.LiveChallenge(ctx, theirs.org.ID, c.Hash); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("another org read this one's challenge: %v", err)
	}
	if _, err := s.ConsumeChallenge(ctx, theirs.org.ID, c.Hash, address, AddressLinked); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("another org consumed this one's challenge: %v", err)
	}
}

// TestReissuingAChallengeSupersedes: asking again is what someone does when the
// first link expired in a scrollback. The old one must stop working, or a
// challenge could be completed after a newer one replaced it.
func TestReissuingAChallengeSupersedes(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300006")
	address := keypair.MustRandom().Address()

	first := challengeFor(t, s, tn, hashOf(5), address)
	second := challengeFor(t, s, tn, hashOf(6), address)

	if _, err := s.LiveChallenge(ctx, tn.org.ID, first.Hash); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("the superseded challenge is still live: %v", err)
	}
	if _, err := s.LiveChallenge(ctx, tn.org.ID, second.Hash); err != nil {
		t.Fatalf("the new challenge is not live: %v", err)
	}
}

func TestLinkCodeRoundTrip(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300007")

	identity := identityOf(t, tn)

	code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identity, 10*time.Minute)
	must(t, err, "issue link code")
	if len(code) != LinkCodeLength {
		t.Fatalf("code = %q", code)
	}

	member, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Discord, "d-777", "ada")
	must(t, err, "claim link code")
	if member != tn.member.ID {
		t.Errorf("claimed for member %d, want %d", member, tn.member.ID)
	}

	// The second handle now speaks for the same member: one human, one wallet.
	viaDiscord, ok, err := s.MemberByIdentity(ctx, tn.org.ID, chat.Discord, "d-777")
	must(t, err, "member by discord identity")
	if !ok || viaDiscord.ID != tn.member.ID {
		t.Fatalf("discord resolved to %+v", viaDiscord)
	}
}

// TestLinkCodeIsSpentOnce: two people racing on one code cannot both win.
func TestLinkCodeIsSpentOnce(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300008")

	identity := identityOf(t, tn)

	code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identity, 10*time.Minute)
	must(t, err, "issue")

	if _, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Discord, "d-888", "a"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Discord, "d-889", "b"); !errors.Is(err, ErrNoLinkCode) {
		t.Fatalf("second claim error = %v, want ErrNoLinkCode", err)
	}
}

func TestUnknownLinkCodeIsRefused(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "-300009")

	if _, err := s.ClaimLinkCode(context.Background(), tn.org.ID, "AAAAAAAA",
		chat.Discord, "d-1", "a"); !errors.Is(err, ErrNoLinkCode) {
		t.Fatalf("error = %v, want ErrNoLinkCode", err)
	}
}

// TestLinkCodesAreUnguessable is a shape check, not a statistical one: the code
// is a bearer credential for as long as it lives, so it must come from a random
// source rather than a counter.
func TestLinkCodesAreUnguessable(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300010")

	identity := identityOf(t, tn)

	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identity, time.Minute)
		must(t, err, "issue")
		if seen[code] {
			t.Fatalf("code %q was issued twice in %d tries", code, i+1)
		}
		seen[code] = true
		for _, r := range code {
			if !containsRune(linkCodeAlphabet, r) {
				t.Fatalf("code %q contains %q, which is not in the reduced alphabet", code, r)
			}
		}
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// grantParams builds a request that passes every check, so a test can break one
// thing at a time.
func grantParams(t *testing.T, s *Store, tn *tenant) GrantParams {
	t.Helper()
	// The treasury gate is answered by the database now, so satisfying it means
	// linking one rather than setting a field.
	linkTreasury(t, s, tn, 1)
	return grantParamsWithoutATreasury(t, s, tn)
}

// grantParamsWithoutATreasury does everything except prove control of money,
// which is the one check the workspace cannot talk its way past.
func grantParamsWithoutATreasury(t *testing.T, s *Store, tn *tenant) GrantParams {
	t.Helper()
	ctx := context.Background()

	identity := identityOf(t, tn)

	// Age both the identity and the workspace past their minimums.
	_, err := testPool.Exec(ctx,
		`UPDATE user_identities SET linked_at = now() - interval '1 day' WHERE id = $1`,
		int64(identity))
	must(t, err, "age the identity")
	_, err = testPool.Exec(ctx,
		`UPDATE orgs SET created_at = now() - interval '7 days' WHERE id = $1`,
		int64(tn.org.ID))
	must(t, err, "age the workspace")

	must(t, s.SetEnrollmentPolicy(ctx, tn.org.ID, money.MustParse("100"), 25), "enable provisioning")

	cost := money.MustParse("1.5")
	return GrantParams{
		Org: tn.org.ID, Member: tn.member.ID, Identity: identity,
		Address: keypair.MustRandom().Address(), ReserveCost: cost,
		SponsorBalance: cost * FloatFloorMultiple,
	}
}

func TestGrantEnrollment(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300011")
	p := grantParams(t, s, tn)

	must(t, s.GrantEnrollment(ctx, p), "grant")

	got, ok, err := s.GrantFor(ctx, tn.org.ID, p.Address)
	must(t, err, "grant for")
	if !ok || got.ReserveCost != p.ReserveCost {
		t.Fatalf("grant = %+v (found %v)", got, ok)
	}
}

// TestOneGrantPerMemberEver is a hard cap rather than a rate limit: nobody needs
// the operator to pay for a second account for them.
func TestOneGrantPerMemberEver(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300012")
	p := grantParams(t, s, tn)

	must(t, s.GrantEnrollment(ctx, p), "first grant")

	again := p
	again.Address = keypair.MustRandom().Address()
	if err := s.GrantEnrollment(ctx, again); !errors.Is(err, ErrAlreadyGranted) {
		t.Fatalf("error = %v, want ErrAlreadyGranted", err)
	}
}

// TestProvisioningIsOffByDefault is the anti-abuse gate. A stranger who adds the
// bot somewhere has no treasury to prove and therefore no budget to spend, so a
// drive-by install costs the operator nothing.
func TestProvisioningIsOffByDefault(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300013")
	p := grantParams(t, s, tn)

	// Undo what grantParams enabled, back to how a fresh workspace starts.
	must(t, s.SetEnrollmentPolicy(ctx, tn.org.ID, 0, 25), "disable provisioning")

	if err := s.GrantEnrollment(ctx, p); !errors.Is(err, ErrProvisioningOff) {
		t.Fatalf("error = %v, want ErrProvisioningOff", err)
	}
}

// TestProvisioningNeedsAVerifiedTreasury: enabling a budget is not enough. The
// workspace has to have proved it controls money of its own first.
func TestProvisioningNeedsAVerifiedTreasury(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "-300014")
	p := grantParamsWithoutATreasury(t, s, tn)

	if err := s.GrantEnrollment(context.Background(), p); !errors.Is(err, ErrProvisioningOff) {
		t.Fatalf("error = %v, want ErrProvisioningOff", err)
	}
}

// TestFloatFloorStopsProvisioningBeforeItFails: the operator must be able to
// cover many more grants before making another promise.
func TestFloatFloorStopsProvisioningBeforeItFails(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "-300015")
	p := grantParams(t, s, tn)
	p.SponsorBalance = p.ReserveCost * (FloatFloorMultiple - 1)

	if err := s.GrantEnrollment(context.Background(), p); !errors.Is(err, ErrFloatTooLow) {
		t.Fatalf("error = %v, want ErrFloatTooLow", err)
	}
}

// TestNewIdentitiesCannotBeProvisioned: a chat account created seconds ago
// asking for a sponsored wallet is the shape of a script, not a person.
func TestNewIdentitiesCannotBeProvisioned(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300016")
	p := grantParams(t, s, tn)

	_, err := testPool.Exec(ctx,
		`UPDATE user_identities SET linked_at = now() WHERE id = $1`, int64(p.Identity))
	must(t, err, "make the identity new again")

	if err := s.GrantEnrollment(ctx, p); !errors.Is(err, ErrTooNew) {
		t.Fatalf("error = %v, want ErrTooNew", err)
	}
}

func TestNewWorkspacesCannotProvision(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300017")
	p := grantParams(t, s, tn)

	_, err := testPool.Exec(ctx,
		`UPDATE orgs SET created_at = now() WHERE id = $1`, int64(tn.org.ID))
	must(t, err, "make the workspace new again")

	if err := s.GrantEnrollment(ctx, p); !errors.Is(err, ErrTooNew) {
		t.Fatalf("error = %v, want ErrTooNew", err)
	}
}

func TestDailyCapIsEnforced(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300018")
	p := grantParams(t, s, tn)
	must(t, s.SetEnrollmentPolicy(ctx, tn.org.ID, money.MustParse("100"), 1), "cap at one")

	must(t, s.GrantEnrollment(ctx, p), "first grant")

	// A different member in the same workspace, so the per-member cap is not
	// what refuses this one.
	other, err := s.EnsureMember(ctx, tn.org.ID, chat.Telegram, "u-capped", "bob")
	must(t, err, "second member")
	var identity IdentityID
	must(t, testPool.QueryRow(ctx,
		`SELECT id FROM user_identities WHERE org_id = $1 AND member_id = $2 LIMIT 1`,
		int64(tn.org.ID), int64(other.ID)).Scan(&identity), "identity")
	_, err = testPool.Exec(ctx,
		`UPDATE user_identities SET linked_at = now() - interval '1 day' WHERE id = $1`,
		int64(identity))
	must(t, err, "age the identity")

	second := p
	second.Member = other.ID
	second.Identity = identity
	second.Address = keypair.MustRandom().Address()

	if err := s.GrantEnrollment(ctx, second); !errors.Is(err, ErrDailyCapReached) {
		t.Fatalf("error = %v, want ErrDailyCapReached", err)
	}
}

func TestReclaimGrant(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-300019")
	p := grantParams(t, s, tn)
	must(t, s.GrantEnrollment(ctx, p), "grant")

	must(t, s.ReclaimGrant(ctx, tn.org.ID, p.Address), "reclaim")

	got, ok, err := s.GrantFor(ctx, tn.org.ID, p.Address)
	must(t, err, "grant for")
	if !ok || !got.Reclaimed {
		t.Fatalf("grant = %+v", got)
	}

	// Reclaiming twice must not silently succeed: the reserve came back once.
	if err := s.ReclaimGrant(ctx, tn.org.ID, p.Address); err == nil {
		t.Error("a grant was reclaimed twice")
	}
}

// TestClaimMovesAnExistingIdentity is the case the router creates and the
// obvious implementation gets wrong.
//
// By the time anyone runs /claim, the claiming account has already spoken once
// and therefore already has a member of its own — empty, with no address and no
// roles. Inserting a second identity row for it fails on the unique index; what
// has to happen is that the identity moves, and the husk it leaves is removed.
func TestClaimMovesAnExistingIdentity(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-310001")

	address := keypair.MustRandom().Address()
	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID, address, AddressLinked), "prove an address")

	// A second chat account that has already spoken, so it has a member.
	stray, err := s.EnsureMember(ctx, tn.org.ID, chat.Discord, "d-stray", "ada-phone")
	must(t, err, "second account speaks")

	code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identityOf(t, tn), 10*time.Minute)
	must(t, err, "issue")

	got, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Discord, "d-stray", "ada-phone")
	must(t, err, "claim")
	if got != tn.member.ID {
		t.Fatalf("claimed for member %d, want %d", got, tn.member.ID)
	}

	// The second account now speaks for the proved member.
	viaDiscord, ok, err := s.MemberByIdentity(ctx, tn.org.ID, chat.Discord, "d-stray")
	must(t, err, "member by identity")
	if !ok || viaDiscord.ID != tn.member.ID {
		t.Fatalf("resolved to %+v, want member %d", viaDiscord, tn.member.ID)
	}

	// And the husk is gone rather than lingering as a member nobody speaks for.
	if _, err := s.Member(ctx, tn.org.ID, stray.ID); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("the emptied member survived: %v", err)
	}
}

// TestClaimWillNotTakeAProvedAccount: moving an identity that already speaks for
// someone with a linked wallet would take a proved member's handle away on the
// strength of a shared secret. The code is not allowed to do that.
func TestClaimWillNotTakeAProvedAccount(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-310002")

	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID,
		keypair.MustRandom().Address(), AddressLinked), "prove the issuer")

	// Another member, also proved, speaking through their own account.
	other, err := s.EnsureMember(ctx, tn.org.ID, chat.Discord, "d-proved", "bob")
	must(t, err, "second member")
	must(t, s.SetMemberAddress(ctx, tn.org.ID, other.ID,
		keypair.MustRandom().Address(), AddressLinked), "prove the other")

	code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identityOf(t, tn), 10*time.Minute)
	must(t, err, "issue")

	if _, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Discord, "d-proved", "bob"); err == nil {
		t.Fatal("a proved account was moved to another member by a code")
	}

	// And it still speaks for whom it did.
	still, ok, err := s.MemberByIdentity(ctx, tn.org.ID, chat.Discord, "d-proved")
	must(t, err, "member by identity")
	if !ok || still.ID != other.ID {
		t.Errorf("the proved account moved: %+v", still)
	}
}

// TestClaimingYourOwnCodeIsHarmless: nothing changes, and it is not an error.
func TestClaimingYourOwnCodeIsHarmless(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-310003")
	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID,
		keypair.MustRandom().Address(), AddressLinked), "prove")

	code, err := s.IssueLinkCode(ctx, tn.org.ID, tn.member.ID, identityOf(t, tn), 10*time.Minute)
	must(t, err, "issue")

	got, err := s.ClaimLinkCode(ctx, tn.org.ID, code, chat.Telegram, "u-310003", "ada")
	must(t, err, "claim own code")
	if got != tn.member.ID {
		t.Errorf("claimed for member %d, want %d", got, tn.member.ID)
	}
}
