package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// linkTreasury links a fresh classic treasury to tn's org.
func linkTreasury(t *testing.T, s *Store, tn *tenant, medium int32) Treasury {
	t.Helper()
	tr, err := s.LinkTreasury(context.Background(), LinkTreasuryParams{
		Org:        tn.org.ID,
		Kind:       TreasuryClassic,
		Address:    keypair.MustRandom().Address(),
		Label:      "main",
		Low:        1,
		Medium:     medium,
		High:       medium,
		VerifiedBy: identityOf(t, tn),
	})
	must(t, err, "link treasury")
	return tr
}

func TestLinkTreasuryRoundTrip(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tr-round")

	tr := linkTreasury(t, s, tn, 2)
	if tr.Kind != TreasuryClassic || tr.Medium != 2 || tr.Contract != "" {
		t.Fatalf("linked %+v", tr)
	}

	byID, err := s.Treasury(ctx, tn.org.ID, tr.ID)
	must(t, err, "treasury by id")
	byAddress, err := s.TreasuryByAddress(ctx, tn.org.ID, tr.Address)
	must(t, err, "treasury by address")
	if byID.ID != tr.ID || byAddress.ID != tr.ID {
		t.Fatalf("lookups disagree: %d, %d, %d", tr.ID, byID.ID, byAddress.ID)
	}

	all, err := s.Treasuries(ctx, tn.org.ID)
	must(t, err, "treasuries")
	if len(all) != 1 || all[0].ID != tr.ID {
		t.Fatalf("Treasuries returned %d row(s)", len(all))
	}
}

// TestLinkTreasuryRefreshesRatherThanDuplicating: proving control again is an
// ordinary thing to do after the signer set changes, and it must update the row
// rather than fail or fork it.
func TestLinkTreasuryRefreshesRatherThanDuplicating(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tr-refresh")

	first := linkTreasury(t, s, tn, 2)
	second, err := s.LinkTreasury(ctx, LinkTreasuryParams{
		Org:     tn.org.ID,
		Kind:    TreasuryClassic,
		Address: first.Address,
		Label:   "renamed",
		Low:     1, Medium: 3, High: 3,
		VerifiedBy: identityOf(t, tn),
	})
	must(t, err, "relink treasury")

	if second.ID != first.ID {
		t.Fatalf("relinking forked the treasury: %d then %d", first.ID, second.ID)
	}
	if second.Medium != 3 || second.Label != "renamed" {
		t.Fatalf("relinking did not refresh: %+v", second)
	}
	if second.RefreshedAt.Before(first.RefreshedAt) {
		t.Errorf("refreshed_at went backwards: %v then %v", first.RefreshedAt, second.RefreshedAt)
	}
}

// TestTreasuryCannotBeTakenOverByAnotherOrg is the one that matters. An address
// belongs to the org that proved control of it; letting a second org claim it
// would post every payment to that account twice, in two sets of books, and the
// duplicate would read like money appearing from nowhere.
func TestTreasuryCannotBeTakenOverByAnotherOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "tr-owner")
	b := newTenant(t, s, "tr-thief")

	tr := linkTreasury(t, s, a, 1)

	_, err := s.LinkTreasury(ctx, LinkTreasuryParams{
		Org:     b.org.ID,
		Kind:    TreasuryClassic,
		Address: tr.Address,
		Low:     1, Medium: 1, High: 1,
		VerifiedBy: identityOf(t, b),
	})
	if !errors.Is(err, ErrNoTreasury) {
		t.Fatalf("a second org claimed a linked treasury: %v", err)
	}

	// And it is still the first org's, unchanged.
	still, err := s.Treasury(ctx, a.org.ID, tr.ID)
	must(t, err, "treasury after the attempt")
	if still.Org != a.org.ID {
		t.Fatalf("treasury moved to org %d", still.Org)
	}
}

func TestTreasuryReadsAreScopedToTheirOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "tr-scope-a")
	b := newTenant(t, s, "tr-scope-b")

	tr := linkTreasury(t, s, a, 1)

	if _, err := s.Treasury(ctx, b.org.ID, tr.ID); !errors.Is(err, ErrNoTreasury) {
		t.Errorf("Treasury across orgs: %v", err)
	}
	if _, err := s.TreasuryByAddress(ctx, b.org.ID, tr.Address); !errors.Is(err, ErrNoTreasury) {
		t.Errorf("TreasuryByAddress across orgs: %v", err)
	}
	if got, err := s.Treasuries(ctx, b.org.ID); err != nil || len(got) != 0 {
		t.Errorf("Treasuries across orgs: %d row(s), %v", len(got), err)
	}
	if got, err := s.CachedSignerSet(ctx, b.org.ID, tr.ID); err != nil || len(got) != 0 {
		t.Errorf("CachedSignerSet across orgs: %d signer(s), %v", len(got), err)
	}
	if err := s.SaveSignerSet(ctx, b.org.ID, tr.ID, nil, 1, 1, 1); !errors.Is(err, ErrNoTreasury) {
		t.Errorf("SaveSignerSet across orgs: %v", err)
	}
}

// TestSaveSignerSetReplaces: a member removed on chain has to disappear here
// too. A merge that only ever adds would keep naming an ex-signer as someone
// the proposal is still waiting on.
func TestSaveSignerSetReplaces(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tr-signers")
	tr := linkTreasury(t, s, tn, 2)

	ada := keypair.MustRandom().Address()
	bo := keypair.MustRandom().Address()
	cy := keypair.MustRandom().Address()

	must(t, s.SaveSignerSet(ctx, tn.org.ID, tr.ID,
		map[string]int32{ada: 1, bo: 1, cy: 1}, 1, 2, 3), "save signers")

	got, err := s.CachedSignerSet(ctx, tn.org.ID, tr.ID)
	must(t, err, "cached signer set")
	if len(got) != 3 || got[ada] != 1 {
		t.Fatalf("cached set = %v", got)
	}

	// Cy is removed on chain and the threshold drops.
	must(t, s.SaveSignerSet(ctx, tn.org.ID, tr.ID,
		map[string]int32{ada: 2, bo: 1}, 1, 2, 2), "save signers again")

	got, err = s.CachedSignerSet(ctx, tn.org.ID, tr.ID)
	must(t, err, "cached signer set again")
	if len(got) != 2 {
		t.Fatalf("cached set kept %d signer(s): %v", len(got), got)
	}
	if _, still := got[cy]; still {
		t.Error("a removed signer survived the refresh")
	}
	if got[ada] != 2 {
		t.Errorf("weight for ada = %d, want 2", got[ada])
	}

	after, err := s.Treasury(ctx, tn.org.ID, tr.ID)
	must(t, err, "treasury after refresh")
	if after.High != 2 {
		t.Errorf("high threshold = %d, want 2", after.High)
	}
}

// TestContractTreasuryKeepsItsContractID: the kind is what tells the signer
// router whether to count weight or ask the contract, and counting weight on a
// contract account produces zero — which reads as "nobody has signed yet"
// rather than "this question does not apply".
func TestContractTreasuryKeepsItsContractID(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tr-contract")

	const contract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"
	tr, err := s.LinkTreasury(ctx, LinkTreasuryParams{
		Org:        tn.org.ID,
		Kind:       TreasuryContract,
		Address:    contract,
		Contract:   contract,
		VerifiedBy: identityOf(t, tn),
	})
	must(t, err, "link contract treasury")

	if tr.Kind != TreasuryContract || tr.Contract != contract {
		t.Fatalf("linked %+v", tr)
	}
}

// TestClassicTreasuryCannotCarryAContractID: the two custody models differ in
// the only place that matters — whether a policy contract can enforce anything
// — so a row that claims both is refused by the database rather than resolved
// by whichever branch happens to run first.
func TestClassicTreasuryCannotCarryAContractID(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "tr-mixed")

	_, err := s.LinkTreasury(context.Background(), LinkTreasuryParams{
		Org:      tn.org.ID,
		Kind:     TreasuryClassic,
		Address:  keypair.MustRandom().Address(),
		Contract: "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE",
		Low:      1, Medium: 1, High: 1,
	})
	if err == nil {
		t.Fatal("a classic treasury was allowed to carry a contract id")
	}
}

// treasuryChallengeFor saves a challenge with the treasury purpose.
func treasuryChallengeFor(t *testing.T, s *Store, tn *tenant, hash, address string) Challenge {
	t.Helper()
	c := Challenge{
		Hash: hash, Org: tn.org.ID, Identity: identityOf(t, tn),
		Purpose: PurposeLinkTreasury, Address: address,
		XDR:       "AAAAAgAAAAA=",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	must(t, s.SaveChallenge(context.Background(), c), "save treasury challenge")
	return c
}

func treasuryParams(tn *tenant, address string) LinkTreasuryParams {
	return LinkTreasuryParams{
		Org: tn.org.ID, Kind: TreasuryClassic, Address: address, Label: "main",
		Low: 1, Medium: 2, High: 2,
	}
}

func TestConsumeTreasuryChallengeLinksAndSpends(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tc-round")

	address := keypair.MustRandom().Address()
	c := treasuryChallengeFor(t, s, tn, hashOf(40), address)

	tr, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(tn, address))
	must(t, err, "consume treasury challenge")
	if tr.Address != address || tr.Medium != 2 {
		t.Fatalf("linked %+v", tr)
	}
	// Whoever presented the proof is who the row records.
	if tr.ID == 0 {
		t.Fatal("no treasury id")
	}

	// The proof is spent: presenting it again is a replay and gets nothing.
	if _, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(tn, address)); !errors.Is(
		err, ErrNoChallenge,
	) {
		t.Fatalf("replaying a spent treasury challenge: %v", err)
	}
}

// TestTreasuryChallengeCannotBindADifferentAccount: the address is matched
// against the challenge rather than read out of what came back, or a signer
// could return a challenge for an account they do control and have it accepted
// as proof of one they do not.
func TestTreasuryChallengeCannotBindADifferentAccount(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "tc-swap")

	proved := keypair.MustRandom().Address()
	wanted := keypair.MustRandom().Address()
	c := treasuryChallengeFor(t, s, tn, hashOf(41), proved)

	if _, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(tn, wanted)); !errors.Is(
		err, ErrNoChallenge,
	) {
		t.Fatalf("bound a different account: %v", err)
	}

	// And the challenge is still live for the account it was actually issued
	// for, so a failed attempt costs the honest path nothing.
	if _, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(tn, proved)); err != nil {
		t.Fatalf("the real account could no longer complete it: %v", err)
	}
}

// TestMemberChallengeCannotLinkATreasury: the purposes are separate rows in the
// same table, and a challenge issued to prove one person's wallet must not be
// redeemable as proof that a group controls its money.
func TestMemberChallengeCannotLinkATreasury(t *testing.T) {
	s := New(testPool)
	tn := newTenant(t, s, "tc-purpose")

	address := keypair.MustRandom().Address()
	c := challengeFor(t, s, tn, hashOf(42), address)

	if _, err := s.ConsumeTreasuryChallenge(
		context.Background(), c.Hash, treasuryParams(tn, address),
	); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a member challenge linked a treasury: %v", err)
	}
}

// TestTreasuryChallengeIsScopedToItsOrg: the org comes from the params, so
// another tenant presenting the same hash must get nothing.
func TestTreasuryChallengeIsScopedToItsOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "tc-scope-a")
	b := newTenant(t, s, "tc-scope-b")

	address := keypair.MustRandom().Address()
	c := treasuryChallengeFor(t, s, a, hashOf(43), address)

	if _, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(b, address)); !errors.Is(
		err, ErrNoChallenge,
	) {
		t.Fatalf("another org spent the challenge: %v", err)
	}
	if _, err := s.ConsumeTreasuryChallenge(ctx, c.Hash, treasuryParams(a, address)); err != nil {
		t.Fatalf("the owning org could no longer complete it: %v", err)
	}
}
