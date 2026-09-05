package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// reclaimFixture is a member with a provisioned account and a sponsor to hand
// it back to.
type reclaimFixture struct {
	svc     *Service
	store   *store.Store
	horizon *signerHorizon
	scope   Scope
	org     store.Org
	member  store.MemberID
	account *keypair.Full
	sponsor *keypair.Full
}

func newReclaimFixture(t *testing.T, name string) *reclaimFixture {
	t.Helper()
	ctx := context.Background()

	h := &signerHorizon{}
	settle, err := settlement.NewWith(h, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}

	sponsor := keypair.MustRandom()
	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{
			Asset:          txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer},
			AssetCode:      "USDC",
			SponsorAddress: sponsor.Address(),
		})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	db := store.New(testPool)
	org := orgFor(t, name)
	user := "u-" + org.Slug
	member, err := db.EnsureMember(ctx, org.ID, chat.Telegram, user, "ada")
	if err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	identity, err := db.IdentityFor(ctx, org.ID, chat.Telegram, user)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	account := keypair.MustRandom()
	if err := db.SetMemberAddress(ctx, org.ID, member.ID, account.Address(),
		store.AddressProvisioned); err != nil {
		t.Fatalf("set member address: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO enrollment_grants (org_id, identity_id, member_id, address, reserve_cost)
		VALUES ($1, $2, $3, $4, $5)`,
		int64(org.ID), int64(identity), int64(member.ID),
		account.Address(), int64(money.MustParse("1.5"))); err != nil {
		t.Fatalf("record grant: %v", err)
	}

	// An empty account: one XLM of reserve and nothing else.
	h.balances = []horizon.Balance{{Balance: "0.0000000", Asset: base.Asset{Type: "native"}}}

	return &reclaimFixture{
		svc: svc, store: db, horizon: h,
		scope:   Scope{Org: org.ID, OwnerRef: "telegram:" + user},
		org:     org,
		member:  member.ID,
		account: account,
		sponsor: sponsor,
	}
}

func TestPrepareReclaimBuildsAMergeToTheSponsor(t *testing.T) {
	f := newReclaimFixture(t, "reclaim plain")

	got, err := f.svc.PrepareReclaim(context.Background(), f.scope, f.account.Address())
	if err != nil {
		t.Fatalf("PrepareReclaim: %v", err)
	}
	if got.Destination != f.sponsor.Address() {
		t.Fatalf("destination = %s, want the sponsor", got.Destination)
	}
	if got.Releasing != settlement.AccountReserve {
		t.Errorf("Releasing = %s, want %s", got.Releasing, settlement.AccountReserve)
	}

	parsed, err := txnbuild.TransactionFromXDR(got.XDR)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, _ := parsed.Transaction()
	ops := tx.Operations()
	if len(ops) != 1 {
		t.Fatalf("%d operation(s), want one merge", len(ops))
	}
	if _, ok := ops[0].(*txnbuild.AccountMerge); !ok {
		t.Fatalf("operation is %T", ops[0])
	}
}

// TestPrepareReclaimRemovesEmptyTrustlines: each one holds half an XLM of
// reserve, and the merge cannot execute while any trustline exists.
func TestPrepareReclaimRemovesEmptyTrustlines(t *testing.T) {
	f := newReclaimFixture(t, "reclaim trustline")
	f.horizon.balances = []horizon.Balance{
		{Balance: "0.0000000", Asset: base.Asset{
			Type: "credit_alphanum4", Code: "USDC", Issuer: testIssuer}},
		{Balance: "0.0000000", Asset: base.Asset{Type: "native"}},
	}

	got, err := f.svc.PrepareReclaim(context.Background(), f.scope, f.account.Address())
	if err != nil {
		t.Fatalf("PrepareReclaim: %v", err)
	}
	if want := settlement.AccountReserve + settlement.BaseReserve; got.Releasing != want {
		t.Errorf("Releasing = %s, want %s", got.Releasing, want)
	}

	parsed, _ := txnbuild.TransactionFromXDR(got.XDR)
	tx, _ := parsed.Transaction()
	ops := tx.Operations()
	if len(ops) != 2 {
		t.Fatalf("%d operation(s), want a trustline removal and a merge", len(ops))
	}
	trust, ok := ops[0].(*txnbuild.ChangeTrust)
	if !ok {
		t.Fatalf("first operation is %T", ops[0])
	}
	// A trustline can only be removed at a limit of zero, and the order matters:
	// the merge cannot execute while the trustline is still there.
	limit, err := money.Parse(trust.Limit)
	if err != nil || limit != 0 {
		t.Errorf("limit = %q (%v), want zero", trust.Limit, err)
	}
	if _, ok := ops[1].(*txnbuild.AccountMerge); !ok {
		t.Fatalf("second operation is %T", ops[1])
	}
}

// TestPrepareReclaimRefusesToDestroyABalance is the refusal that matters.
// Removing a trustline is what makes the merge possible, and a trustline can
// only be removed at zero — so the only way this could "work" would be to give
// the balance up.
func TestPrepareReclaimRefusesToDestroyABalance(t *testing.T) {
	f := newReclaimFixture(t, "reclaim not empty")
	f.horizon.balances = []horizon.Balance{
		{Balance: "42.5000000", Asset: base.Asset{
			Type: "credit_alphanum4", Code: "USDC", Issuer: testIssuer}},
		{Balance: "0.0000000", Asset: base.Asset{Type: "native"}},
	}

	_, err := f.svc.PrepareReclaim(context.Background(), f.scope, f.account.Address())
	if !errors.Is(err, ErrAccountNotEmpty) {
		t.Fatalf("error = %v, want ErrAccountNotEmpty", err)
	}
	// And it names what is in the way, because "cannot be handed back" with no
	// reason is a dead end.
	if got := err.Error(); !strings.Contains(got, "42.5000000") || !strings.Contains(got, "USDC") {
		t.Errorf("error does not name the balance: %s", got)
	}
}

func TestPrepareReclaimRefusesWhatWasNotProvisioned(t *testing.T) {
	f := newReclaimFixture(t, "reclaim not ours")

	// A wallet the member brought themselves. Theirs to keep or close, and not
	// something stelfin should be building a deletion for.
	other := keypair.MustRandom()
	if _, err := f.svc.PrepareReclaim(context.Background(), f.scope, other.Address()); !errors.Is(
		err, ErrNothingToReclaim,
	) {
		t.Fatalf("error = %v, want ErrNothingToReclaim", err)
	}
}

// TestPrepareReclaimRefusesATreasury: a workspace that provisioned an account
// and later made it a treasury would otherwise be one command from having it
// deleted.
func TestPrepareReclaimRefusesATreasury(t *testing.T) {
	f := newReclaimFixture(t, "reclaim treasury")
	ctx := context.Background()

	if _, err := f.store.LinkTreasury(ctx, store.LinkTreasuryParams{
		Org: f.scope.Org, Kind: store.TreasuryClassic,
		Address: f.account.Address(), Low: 1, Medium: 1, High: 1,
	}); err != nil {
		t.Fatalf("link treasury: %v", err)
	}

	if _, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address()); !errors.Is(
		err, ErrCannotReclaim,
	) {
		t.Fatalf("error = %v, want ErrCannotReclaim", err)
	}
}

func TestPrepareReclaimRefusesSubEntriesItCannotClear(t *testing.T) {
	ctx := context.Background()

	t.Run("data entries", func(t *testing.T) {
		f := newReclaimFixture(t, "reclaim data")
		f.horizon.data = map[string]string{"key": "dmFsdWU="}
		if _, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address()); !errors.Is(
			err, ErrCannotReclaim,
		) {
			t.Fatalf("error = %v, want ErrCannotReclaim", err)
		}
	})

	t.Run("sponsoring others", func(t *testing.T) {
		f := newReclaimFixture(t, "reclaim sponsoring")
		f.horizon.sponsoring = 2
		if _, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address()); !errors.Is(
			err, ErrCannotReclaim,
		) {
			t.Fatalf("error = %v, want ErrCannotReclaim", err)
		}
	})
}

// TestSubmitReclaimIsOverTheEnvelopeItShowed: the hash covers the transaction
// and not its signatures, so this is what proves the balance about to be swept
// is still going where the member was told.
func TestSubmitReclaimIsOverTheEnvelopeItShowed(t *testing.T) {
	f := newReclaimFixture(t, "reclaim wrong envelope")
	ctx := context.Background()

	prepared, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address())
	if err != nil {
		t.Fatalf("PrepareReclaim: %v", err)
	}

	// A merge of the same account, signed by the same key, to somewhere else.
	elsewhere, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: f.account.Address(), Sequence: 1,
		},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{
			&txnbuild.AccountMerge{Destination: keypair.MustRandom().Address()},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	signed, err := elsewhere.Sign(network.TestNetworkPassphrase, f.account)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	encoded, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if _, err := f.svc.SubmitReclaim(ctx, f.scope, prepared.Hash, encoded); !errors.Is(
		err, ErrWrongEnvelope,
	) {
		t.Fatalf("a merge to somewhere else was accepted: %v", err)
	}

	// And the prepared one is still live, so a failed attempt costs the honest
	// path nothing.
	if _, err := f.svc.LoadReclaim(ctx, f.scope, prepared.Hash); err != nil {
		t.Fatalf("the real envelope is no longer live: %v", err)
	}
}

func TestSubmitReclaimReleasesTheAccount(t *testing.T) {
	f := newReclaimFixture(t, "reclaim submit")
	ctx := context.Background()

	prepared, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address())
	if err != nil {
		t.Fatalf("PrepareReclaim: %v", err)
	}
	signed := signEnvelope(t, prepared.XDR, f.account)

	result, err := f.svc.SubmitReclaim(ctx, f.scope, prepared.Hash, signed)
	if err != nil {
		t.Fatalf("SubmitReclaim: %v", err)
	}
	if result.Address != f.account.Address() {
		t.Errorf("released %s", result.Address)
	}

	// The member survives, without the address. Removing them would erase who
	// approved what.
	member, err := f.store.Member(ctx, f.scope.Org, f.member)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	if member.Address != "" {
		t.Errorf("the member still holds %s", member.Address)
	}

	grant, ok, err := f.store.GrantFor(ctx, f.scope.Org, f.account.Address())
	if err != nil || !ok {
		t.Fatalf("grant: %v (found %v)", err, ok)
	}
	if !grant.Reclaimed {
		t.Error("the grant was not released")
	}

	// Submitting the same envelope again does nothing. The account is gone;
	// a second attempt can only produce a confusing failure.
	if _, err := f.svc.SubmitReclaim(ctx, f.scope, prepared.Hash, signed); !errors.Is(
		err, store.ErrNoReclaim,
	) {
		t.Fatalf("second submission: %v", err)
	}
}

// TestReclaimIsScopedToItsOwner: this envelope deletes an account, so which
// envelope and whose must not be possible to vary independently.
func TestReclaimIsScopedToItsOwner(t *testing.T) {
	f := newReclaimFixture(t, "reclaim owner")
	ctx := context.Background()

	prepared, err := f.svc.PrepareReclaim(ctx, f.scope, f.account.Address())
	if err != nil {
		t.Fatalf("PrepareReclaim: %v", err)
	}

	elsewhere := Scope{Org: f.scope.Org, OwnerRef: "telegram:someone-else"}
	if _, err := f.svc.LoadReclaim(ctx, elsewhere, prepared.Hash); !errors.Is(
		err, store.ErrNoReclaim,
	) {
		t.Errorf("LoadReclaim for another owner: %v", err)
	}
	if _, err := f.svc.SubmitReclaim(ctx, elsewhere, prepared.Hash,
		signEnvelope(t, prepared.XDR, f.account)); !errors.Is(err, store.ErrNoReclaim) {
		t.Errorf("SubmitReclaim for another owner: %v", err)
	}
}
