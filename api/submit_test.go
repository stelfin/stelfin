package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// signWith returns a fee-bump signer for the given treasury key.
func signWith(kp *keypair.Full) func(*txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error) {
	return func(tx *txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error) {
		return tx.Sign(network.TestNetworkPassphrase, kp)
	}
}

// signProvisionWith returns a provisioning signer for the given treasury key.
func signProvisionWith(kp *keypair.Full) func(*txnbuild.Transaction) (*txnbuild.Transaction, error) {
	return func(tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
		return tx.Sign(network.TestNetworkPassphrase, kp)
	}
}

// issueAndSign prepares a send and signs it as the user would.
func issueAndSign(t *testing.T, f *fixture, userKey *keypair.Full) (*Confirmation, string) {
	t.Helper()

	c, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	parsed, err := txnbuild.TransactionFromXDR(c.XDR)
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
	return c, signedXDR
}

func TestPrepareRecordsAPendingSend(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()

	c, err := f.svc.PrepareSend(ctx, f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	pending, err := f.svc.Pending(ctx, f.owner)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending sends, want 1", len(pending))
	}
	if pending[0].Hash != c.Hash {
		t.Errorf("pending hash = %s, want %s", pending[0].Hash, c.Hash)
	}
	if want := money.MustParse("5000"); pending[0].Amount != want {
		t.Errorf("pending amount = %s, want %s", pending[0].Amount, want)
	}
}

func TestSubmitIssuedTransaction(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	res, err := f.svc.Submit(context.Background(), f.owner, signedXDR,
		treasury.Address(), signWith(treasury))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Ledger != 12 {
		t.Errorf("ledger = %d, want 12", res.Ledger)
	}
}

// TestSubmitRejectsTransactionWeNeverIssued is the attack this whole mechanism
// exists to stop. The treasury pays the fee for anything it fee-bumps, so an
// endpoint that submitted whatever it was handed would let anyone spend the
// treasury's XLM on transactions stelfin never authored.
func TestSubmitRejectsTransactionWeNeverIssued(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()

	// A perfectly valid, correctly signed transaction — just not ours.
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

	_, err = f.svc.Submit(context.Background(), f.owner, xdr, treasury.Address(), signWith(treasury))
	if !errors.Is(err, ErrUnknownTransaction) {
		t.Fatalf("error = %v, want ErrUnknownTransaction", err)
	}
}

// TestSubmitRejectsAnotherUsersTransaction: knowing a valid envelope is not
// authority to spend against it.
func TestSubmitRejectsAnotherUsersTransaction(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	_, err := f.svc.Submit(context.Background(), "someone-else", signedXDR,
		treasury.Address(), signWith(treasury))
	if !errors.Is(err, ErrNotYours) {
		t.Fatalf("error = %v, want ErrNotYours", err)
	}
}

// TestSubmitRejectsReplay: the treasury must not pay to fee-bump the same
// transaction twice.
func TestSubmitRejectsReplay(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()
	treasury := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	if _, err := f.svc.Submit(ctx, f.owner, signedXDR, treasury.Address(), signWith(treasury)); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err := f.svc.Submit(ctx, f.owner, signedXDR, treasury.Address(), signWith(treasury))
	if !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("error = %v, want ErrAlreadySubmitted", err)
	}
}

// TestConcurrentSubmitClaimsOnce: the claim is a conditional UPDATE, so two
// racing submissions cannot both fee-bump.
func TestConcurrentSubmitClaimsOnce(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()
	treasury := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Submit(ctx, f.owner, signedXDR, treasury.Address(), signWith(treasury))
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

func TestSubmitRejectsUnsignedEnvelope(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()

	c, err := f.svc.PrepareSend(context.Background(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	// The XDR exactly as issued — never signed.
	_, err = f.svc.Submit(context.Background(), f.owner, c.XDR, treasury.Address(), signWith(treasury))
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("error = %v, want ErrUnsigned", err)
	}
}

func TestSubmitRejectsExpiredTransaction(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()
	treasury := keypair.MustRandom()
	c, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	// Age the claim the way elapsed time would: both timestamps move back
	// together, so the row stays valid under the schema's own sanity check
	// that a claim is never born already expired.
	if _, err := testPool.Exec(ctx, `
		UPDATE pending_sends
		   SET created_at = created_at - interval '1 hour',
		       expires_at = expires_at - interval '1 hour'
		 WHERE hash = $1`, c.Hash,
	); err != nil {
		t.Fatalf("age pending send: %v", err)
	}

	_, err := f.svc.Submit(ctx, f.owner, signedXDR, treasury.Address(), signWith(treasury))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("error = %v, want ErrExpired", err)
	}
}

// TestSubmitRejectsAFeeBump: building the outer envelope is the treasury's job.
// Accepting a client-built fee-bump would mean submitting an envelope whose fee
// account we never chose.
func TestSubmitRejectsAFeeBump(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	userKey := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, userKey)

	parsed, _ := txnbuild.TransactionFromXDR(signedXDR)
	inner, _ := parsed.Transaction()
	bump, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner: inner, FeeAccount: keypair.MustRandom().Address(), BaseFee: 10_000,
	})
	if err != nil {
		t.Fatalf("build fee bump: %v", err)
	}
	signedBump, err := bump.Sign(network.TestNetworkPassphrase, treasury)
	if err != nil {
		t.Fatalf("sign bump: %v", err)
	}
	bumpXDR, err := signedBump.Base64()
	if err != nil {
		t.Fatalf("encode bump: %v", err)
	}

	_, err = f.svc.Submit(context.Background(), f.owner, bumpXDR, treasury.Address(), signWith(treasury))
	if err == nil {
		t.Fatal("expected an error: the client must not build the outer envelope")
	}
}

func TestPendingExcludesSubmittedAndExpired(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()
	treasury := keypair.MustRandom()
	_, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	if _, err := f.svc.Submit(ctx, f.owner, signedXDR, treasury.Address(), signWith(treasury)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	pending, err := f.svc.Pending(ctx, f.owner)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("got %d pending sends after submission, want 0", len(pending))
	}
}

func TestPendingExpiryMatchesTransactionTimebounds(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()

	c, err := f.svc.PrepareSend(ctx, f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	parsed, _ := txnbuild.TransactionFromXDR(c.XDR)
	tx, _ := parsed.Transaction()
	want := time.Unix(tx.Timebounds().MaxTime, 0).UTC()

	pending, err := f.svc.Pending(ctx, f.owner)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending sends, want 1", len(pending))
	}
	// The claim must not outlive the transaction, or a submission accepted
	// here would be refused by the network anyway.
	if !pending[0].ExpiresAt.UTC().Equal(want) {
		t.Errorf("claim expires at %s, transaction at %s", pending[0].ExpiresAt.UTC(), want)
	}
}
