package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// newUnenrolledService returns a Service and a fresh phone number with no
// stellar_accounts row — the state enrollment exists to move a user out of.
func newUnenrolledService(t *testing.T) (*Service, Scope) {
	t.Helper()
	settle, err := settlement.NewWith(&fakeHorizon{sequence: 5}, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{Asset: txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}, AssetCode: "USDC"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	owner := Scope{Org: orgFor(t, "/enroll").ID, OwnerRef: ownerFor(t)}
	openProvisioning(t, owner)
	return svc, owner
}

// openProvisioning puts a workspace and one of its members in the state in
// which the operator will pay for an account: a proved treasury, a budget, and
// a member and a workspace both old enough.
//
// The ages are written straight to the database. Every other condition is
// reached the way a workspace reaches it; nothing but time makes something old.
func openProvisioning(t *testing.T, owner Scope) {
	t.Helper()
	ctx := context.Background()
	db := store.New(testPool)

	if _, err := db.LinkTreasury(ctx, store.LinkTreasuryParams{
		Org: owner.Org, Kind: store.TreasuryClassic,
		Address: keypair.MustRandom().Address(), Label: "main",
		Low: 1, Medium: 1, High: 1,
	}); err != nil {
		t.Fatalf("link treasury: %v", err)
	}
	if err := db.SetEnrollmentPolicy(ctx, owner.Org, ProvisionReserve*25, 25); err != nil {
		t.Fatalf("set enrolment policy: %v", err)
	}
	seen(t, owner)
	if _, err := testPool.Exec(ctx,
		`UPDATE orgs SET created_at = now() - interval '2 hours' WHERE id = $1`,
		int64(owner.Org)); err != nil {
		t.Fatalf("age the workspace: %v", err)
	}
}

// seen makes owner a member of their org, first seen long enough ago.
func seen(t *testing.T, owner Scope) {
	t.Helper()
	ctx := context.Background()
	channel, userID, _ := strings.Cut(owner.OwnerRef, ":")
	if _, err := store.New(testPool).EnsureMember(ctx, owner.Org, chat.Channel(channel), userID, "ada"); err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE user_identities SET linked_at = now() - interval '1 hour'
		 WHERE org_id = $1 AND channel = $2 AND channel_user_id = $3`,
		int64(owner.Org), channel, userID); err != nil {
		t.Fatalf("age the identity: %v", err)
	}
}

// prepareAndSign runs PrepareEnrollment and signs the returned envelope as
// the device would.
func prepareAndSign(t *testing.T, svc *Service, owner Scope, treasuryAddr string, userKey *keypair.Full) (*Enrollment, string) {
	t.Helper()
	e, err := svc.PrepareEnrollment(context.Background(), owner, userKey.Address(), treasuryAddr)
	if err != nil {
		t.Fatalf("PrepareEnrollment: %v", err)
	}
	parsed, err := txnbuild.TransactionFromXDR(e.XDR)
	if err != nil {
		t.Fatalf("parse issued XDR: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("issued envelope is not a plain transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, userKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	signedXDR, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode signed: %v", err)
	}
	return e, signedXDR
}

func TestPrepareEnrollment(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	userKey := keypair.MustRandom()
	treasury := keypair.MustRandom()

	e, err := svc.PrepareEnrollment(context.Background(), owner, userKey.Address(), treasury.Address())
	if err != nil {
		t.Fatalf("PrepareEnrollment: %v", err)
	}
	if e.Address != userKey.Address() {
		t.Errorf("address = %s, want %s", e.Address, userKey.Address())
	}
	if e.XDR == "" || e.Hash == "" {
		t.Error("enrollment carries no transaction")
	}

	parsed, err := txnbuild.TransactionFromXDR(e.XDR)
	if err != nil {
		t.Fatalf("parse XDR: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("XDR is not a plain transaction")
	}
	ops := tx.Operations()
	if len(ops) != 4 {
		t.Fatalf("envelope carries %d operations, want exactly 4", len(ops))
	}
	if _, ok := ops[0].(*txnbuild.BeginSponsoringFutureReserves); !ok {
		t.Errorf("op 0 = %T, want BeginSponsoringFutureReserves", ops[0])
	}
	create, ok := ops[1].(*txnbuild.CreateAccount)
	if !ok || create.Destination != userKey.Address() {
		t.Errorf("op 1 = %+v, want CreateAccount for %s", ops[1], userKey.Address())
	}
	if _, ok := ops[2].(*txnbuild.ChangeTrust); !ok {
		t.Errorf("op 2 = %T, want ChangeTrust", ops[2])
	}
	if _, ok := ops[3].(*txnbuild.EndSponsoringFutureReserves); !ok {
		t.Errorf("op 3 = %T, want EndSponsoringFutureReserves", ops[3])
	}
}

// TestPrepareEnrollmentRejectsAlreadyEnrolled: re-enrolling a phone number
// that already has an account must not silently produce a second one.
func TestPrepareEnrollmentRejectsAlreadyEnrolled(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()

	_, err := f.svc.PrepareEnrollment(context.Background(), f.scope, keypair.MustRandom().Address(), treasury.Address())
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("error = %v, want ErrAlreadyEnrolled", err)
	}
}

// TestPrepareEnrollmentRetrySupersedesThePrevious: requesting enrollment
// twice before either is submitted must not collide — even though the two
// calls build different transactions (each carries its own wall-clock
// timeout), the second must replace the first as the one outstanding
// enrollment for the phone number, not pile up alongside it.
func TestPrepareEnrollmentRetrySupersedesThePrevious(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	userKey := keypair.MustRandom()
	treasury := keypair.MustRandom()
	ctx := context.Background()

	first, err := svc.PrepareEnrollment(ctx, owner, userKey.Address(), treasury.Address())
	if err != nil {
		t.Fatalf("first PrepareEnrollment: %v", err)
	}
	second, err := svc.PrepareEnrollment(ctx, owner, userKey.Address(), treasury.Address())
	if err != nil {
		t.Fatalf("second PrepareEnrollment: %v", err)
	}

	var pending int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM pending_enrollments
		  WHERE org_id = $1 AND owner_ref = $2 AND submitted_at IS NULL`,
		int64(owner.Org), owner.OwnerRef,
	).Scan(&pending); err != nil {
		t.Fatalf("count pending enrollments: %v", err)
	}
	if pending != 1 {
		t.Errorf("%d outstanding enrollments for %s, want exactly 1", pending, owner.OwnerRef)
	}

	// The first attempt's hash must no longer be a live claim: the row was
	// superseded in place, so even a properly signed envelope for it is
	// unrecognised now, not merely unsigned or expired.
	if first.Hash == second.Hash {
		return // same-second race; nothing more to assert
	}
	parsed, err := txnbuild.TransactionFromXDR(first.XDR)
	if err != nil {
		t.Fatalf("parse first XDR: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("first XDR is not a plain transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, userKey)
	if err != nil {
		t.Fatalf("sign first envelope: %v", err)
	}
	signedXDR, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode signed first envelope: %v", err)
	}
	if _, err := svc.SubmitEnrollment(ctx, owner, signedXDR, treasury.Address(), signProvisionWith(treasury)); !errors.Is(err, ErrUnknownTransaction) {
		t.Errorf("error = %v, want ErrUnknownTransaction: the superseded attempt must not still be claimable", err)
	}
}

func TestSubmitEnrollment(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	userKey := keypair.MustRandom()
	treasury := keypair.MustRandom()

	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), userKey)

	res, err := svc.SubmitEnrollment(context.Background(), owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
	if err != nil {
		t.Fatalf("SubmitEnrollment: %v", err)
	}
	if res.Ledger != 7 {
		t.Errorf("ledger = %d, want 7", res.Ledger)
	}
	if res.Address != userKey.Address() {
		t.Errorf("address = %s, want %s", res.Address, userKey.Address())
	}

	// The point of the whole flow: the phone number now resolves to a
	// stellar_accounts row, the same lookup PrepareSend depends on.
	got, err := svc.stellarAddress(context.Background(), owner)
	if err != nil {
		t.Fatalf("stellarAddress after enrollment: %v", err)
	}
	if got != userKey.Address() {
		t.Errorf("resolved address = %s, want %s", got, userKey.Address())
	}
}

func TestSubmitEnrollmentRejectsUnsignedEnvelope(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()

	e, err := svc.PrepareEnrollment(context.Background(), owner, keypair.MustRandom().Address(), treasury.Address())
	if err != nil {
		t.Fatalf("PrepareEnrollment: %v", err)
	}

	_, err = svc.SubmitEnrollment(context.Background(), owner, e.XDR, treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("error = %v, want ErrUnsigned", err)
	}
}

func TestSubmitEnrollmentRejectsForeignTransaction(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()

	attacker := keypair.MustRandom()
	foreign, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: attacker.Address(), Sequence: 1},
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 100}},
		BaseFee:              1000,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		t.Fatalf("build foreign transaction: %v", err)
	}
	signed, err := foreign.Sign(network.TestNetworkPassphrase, attacker)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	xdr, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, err = svc.SubmitEnrollment(context.Background(), owner, xdr, treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrUnknownTransaction) {
		t.Fatalf("error = %v, want ErrUnknownTransaction", err)
	}
}

func TestSubmitEnrollmentRejectsAnotherMembersTransaction(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()
	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), keypair.MustRandom())

	stranger := Scope{Org: owner.Org, OwnerRef: "telegram:someone-else"}
	_, err := svc.SubmitEnrollment(context.Background(), stranger, signedXDR,
		treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrNotYours) {
		t.Fatalf("error = %v, want ErrNotYours", err)
	}
}

// TestSubmitEnrollmentRejectsAnotherOrgsTransaction: a hash from another tenant
// must look like an unknown transaction, not like one belonging to someone
// else — the second answer confirms it exists.
func TestSubmitEnrollmentRejectsAnotherOrgsTransaction(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()
	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), keypair.MustRandom())

	elsewhere := Scope{Org: orgFor(t, "/elsewhere").ID, OwnerRef: owner.OwnerRef}
	_, err := svc.SubmitEnrollment(context.Background(), elsewhere, signedXDR,
		treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrUnknownTransaction) {
		t.Fatalf("error = %v, want ErrUnknownTransaction", err)
	}
}

func TestSubmitEnrollmentRejectsReplay(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()
	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), keypair.MustRandom())

	if _, err := svc.SubmitEnrollment(context.Background(), owner, signedXDR, treasury.Address(), signProvisionWith(treasury)); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err := svc.SubmitEnrollment(context.Background(), owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("error = %v, want ErrAlreadySubmitted", err)
	}
}

func TestSubmitEnrollmentRejectsExpired(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	ctx := context.Background()
	treasury := keypair.MustRandom()
	e, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), keypair.MustRandom())

	if _, err := testPool.Exec(ctx, `
		UPDATE pending_enrollments
		   SET created_at = created_at - interval '1 hour',
		       expires_at = expires_at - interval '1 hour'
		 WHERE hash = $1`, e.Hash,
	); err != nil {
		t.Fatalf("age pending enrollment: %v", err)
	}

	_, err := svc.SubmitEnrollment(ctx, owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("error = %v, want ErrExpired", err)
	}
}

// TestConcurrentSubmitEnrollmentClaimsOnce: the claim is a conditional
// UPDATE, so two racing submissions cannot both create an account.
func TestConcurrentSubmitEnrollmentClaimsOnce(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	ctx := context.Background()
	treasury := keypair.MustRandom()
	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), keypair.MustRandom())

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.SubmitEnrollment(ctx, owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	accepted := 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrAlreadySubmitted):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if accepted != 1 {
		t.Errorf("%d submissions were accepted, want exactly 1", accepted)
	}
}

// limitedService is newUnenrolledService with the fake network kept in hand,
// and provisioning left exactly as a new workspace has it: off.
func limitedService(t *testing.T) (*Service, *fakeHorizon, Scope) {
	t.Helper()
	horizon := &fakeHorizon{sequence: 5}
	settle, err := settlement.NewWith(horizon, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{Asset: txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}, AssetCode: "USDC"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, horizon, Scope{Org: orgFor(t, "/limited").ID, OwnerRef: ownerFor(t)}
}

// TestEnrollmentIsOffUntilAWorkspaceTurnsItOn: every account costs the operator
// reserve it does not get back on its own. A workspace that has proved nothing
// and asked for nothing must not be able to spend it — otherwise adding the bot
// to a group is all it takes to drain the float.
func TestEnrollmentIsOffUntilAWorkspaceTurnsItOn(t *testing.T) {
	svc, horizon, owner := limitedService(t)
	seen(t, owner)
	treasury := keypair.MustRandom()

	_, err := svc.PrepareEnrollment(context.Background(), owner,
		keypair.MustRandom().Address(), treasury.Address())
	if !errors.Is(err, store.ErrProvisioningOff) {
		t.Fatalf("error = %v, want ErrProvisioningOff", err)
	}

	// A budget alone is not enough: the treasury is the gate.
	if err := store.New(testPool).SetEnrollmentPolicy(context.Background(),
		owner.Org, ProvisionReserve*25, 25); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	_, err = svc.PrepareEnrollment(context.Background(), owner,
		keypair.MustRandom().Address(), treasury.Address())
	if !errors.Is(err, store.ErrProvisioningOff) {
		t.Fatalf("with a budget and no treasury: error = %v, want ErrProvisioningOff", err)
	}
	if n := horizon.submits.Load(); n != 0 {
		t.Fatalf("%d transactions reached the network", n)
	}
}

// TestSubmitEnrollmentTakesTheGrantBeforeSigning is the limiter doing its job
// at the only moment it can: before the operator's signature goes on. A member
// first seen moments ago is refused, nothing reaches the network, and — because
// nothing was signed — the same envelope is good again once they are not new.
func TestSubmitEnrollmentTakesTheGrantBeforeSigning(t *testing.T) {
	svc, horizon, owner := limitedService(t)
	openProvisioning(t, owner)
	ctx := context.Background()
	treasury := keypair.MustRandom()
	userKey := keypair.MustRandom()

	// Seen just now.
	if _, err := testPool.Exec(ctx,
		`UPDATE user_identities SET linked_at = now() WHERE org_id = $1`, int64(owner.Org)); err != nil {
		t.Fatalf("reset identity age: %v", err)
	}

	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), userKey)
	_, err := svc.SubmitEnrollment(ctx, owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
	if !errors.Is(err, store.ErrTooNew) {
		t.Fatalf("error = %v, want ErrTooNew", err)
	}
	if n := horizon.submits.Load(); n != 0 {
		t.Fatalf("a refused enrolment reached the network %d time(s)", n)
	}

	seen(t, owner) // old enough now
	res, err := svc.SubmitEnrollment(ctx, owner, signedXDR, treasury.Address(), signProvisionWith(treasury))
	if err != nil {
		t.Fatalf("SubmitEnrollment once old enough: %v", err)
	}
	if res.Address != userKey.Address() || horizon.submits.Load() != 1 {
		t.Fatalf("result = %+v after %d submissions", res, horizon.submits.Load())
	}

	// The grant is what /reclaim looks for, and what makes it one per member.
	grant, ok, err := store.New(testPool).GrantFor(ctx, owner.Org, userKey.Address())
	if err != nil || !ok {
		t.Fatalf("grant for the new account: %v (found %v)", err, ok)
	}
	if grant.ReserveCost != ProvisionReserve {
		t.Errorf("grant records %s of reserve, want %s", grant.ReserveCost, ProvisionReserve)
	}
}
