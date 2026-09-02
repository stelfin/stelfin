package signer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/signer"
)

const testNet = network.TestNetworkPassphrase

// keyFor derives a keypair from a fixed seed so the addresses in this file are
// real ones with valid checksums. Hand-written G... constants have been wrong
// here before, and a bad checksum fails in a way that looks like a logic bug.
func keyFor(t *testing.T, b byte) *keypair.Full {
	t.Helper()
	var raw [32]byte
	for i := range raw {
		raw[i] = b
	}
	kp, err := keypair.FromRawSeed(raw)
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	return kp
}

func payment(t *testing.T, source string) *txnbuild.Transaction {
	t.Helper()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: source, Sequence: 1},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: source,
			Amount:      "1",
			Asset:       txnbuild.NativeAsset{},
		}},
	})
	if err != nil {
		t.Fatalf("build transaction: %v", err)
	}
	return tx
}

func sign(t *testing.T, tx *txnbuild.Transaction, kps ...*keypair.Full) *txnbuild.Transaction {
	t.Helper()
	signed, err := tx.Sign(testNet, kps...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// lookup is a TreasuryLookup that answers with one fixed set.
type lookup struct {
	set signer.Set
	err error
}

func (l lookup) SignerSet(context.Context, ledger.OrgID, string) (signer.Set, error) {
	return l.set, l.err
}

func TestLocalRefusesMainnetWithoutAcknowledgement(t *testing.T) {
	kp := keyFor(t, 1)

	if _, err := signer.NewLocal(kp.Seed(), network.PublicNetworkPassphrase, false); !errors.Is(
		err, signer.ErrMainnetKeyInProcess,
	) {
		t.Fatalf("public network without acknowledgement: got %v, want ErrMainnetKeyInProcess", err)
	}
	if _, err := signer.NewLocal(kp.Seed(), network.PublicNetworkPassphrase, true); err != nil {
		t.Fatalf("public network with acknowledgement: %v", err)
	}
	if _, err := signer.NewLocal(kp.Seed(), testNet, false); err != nil {
		t.Fatalf("test network: %v", err)
	}
}

func TestExternalCountsOnlyTheAccountsOwnSigners(t *testing.T) {
	a, b, c := keyFor(t, 2), keyFor(t, 3), keyFor(t, 4)
	stranger := keyFor(t, 5)

	set := signer.Set{
		Signers: map[string]int32{a.Address(): 1, b.Address(): 1, c.Address(): 1},
		Medium:  2,
	}
	ext := signer.NewExternal(set)

	cases := []struct {
		name     string
		signers  []*keypair.Full
		wantHave int32
		wantDone bool
	}{
		{"unsigned", nil, 0, false},
		{"one of three", []*keypair.Full{a}, 1, false},
		{"two of three", []*keypair.Full{a, b}, 2, true},
		{"all three", []*keypair.Full{a, b, c}, 3, true},
		// A stranger's signature is well formed and verifies against its own
		// key. It is still worth nothing, because it is not on the account.
		{"stranger alone", []*keypair.Full{stranger}, 0, false},
		{"stranger padding a real signer", []*keypair.Full{a, stranger}, 1, false},
		// The same key twice is the same key. Counting it twice would announce
		// a proposal ready that the network will reject.
		{"one signer twice", []*keypair.Full{a, a}, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := sign(t, payment(t, a.Address()), tc.signers...)

			got, err := ext.Sign(context.Background(), signer.Request{
				Purpose: signer.PurposeTreasury,
				Network: testNet,
				Account: a.Address(),
				Tx:      tx,
			})
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			if got.Have != tc.wantHave {
				t.Errorf("Have = %d, want %d", got.Have, tc.wantHave)
			}
			if got.Need != 2 {
				t.Errorf("Need = %d, want 2", got.Need)
			}
			if got.Complete != tc.wantDone {
				t.Errorf("Complete = %v, want %v", got.Complete, tc.wantDone)
			}
		})
	}
}

func TestExternalIgnoresASignatureForAnotherNetwork(t *testing.T) {
	a := keyFor(t, 6)
	tx, err := payment(t, a.Address()).Sign(network.PublicNetworkPassphrase, a)
	if err != nil {
		t.Fatalf("sign for the wrong network: %v", err)
	}

	got, err := signer.NewExternal(signer.Set{
		Signers: map[string]int32{a.Address(): 1},
		Medium:  1,
	}).Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: a.Address(),
		Tx:      tx,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got.Have != 0 || got.Complete {
		t.Fatalf("Have = %d, Complete = %v; a signature over another network's hash must count for nothing",
			got.Have, got.Complete)
	}
}

func TestExternalRefusesAnAccountWithNoSigners(t *testing.T) {
	a := keyFor(t, 7)
	_, err := signer.NewExternal(signer.Set{Medium: 1}).Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: a.Address(),
		Tx:      payment(t, a.Address()),
	})
	if err == nil {
		t.Fatal("an empty signer set must be an error, not a weight of zero")
	}
}

func TestRouterNeverSignsForATreasury(t *testing.T) {
	operator := keyFor(t, 8)
	member := keyFor(t, 9)

	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	// A treasury with a single signer of weight 1 and a threshold of 1: the
	// case most likely to tempt a shortcut, because "one signer" reads like
	// "the bot can just sign it".
	r, err := signer.NewRouter(signer.Config{
		Network:  testNet,
		Operator: local,
		Treasuries: lookup{set: signer.Set{
			Signers: map[string]int32{member.Address(): 1},
			Medium:  1,
		}},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	tx := payment(t, member.Address())
	got, err := r.Sign(context.Background(), signer.Request{
		Org:     ledger.OrgID(1),
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: member.Address(),
		Tx:      tx,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got.Complete {
		t.Fatal("the router completed a treasury signature; it must never hold a member's key")
	}
	if n := len(got.Tx.Signatures()); n != 0 {
		t.Fatalf("the router added %d signature(s) to a treasury envelope", n)
	}
}

func TestRouterSignsSponsorshipsAndFeeBumps(t *testing.T) {
	operator := keyFor(t, 10)
	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	r, err := signer.NewRouter(signer.Config{
		Network:    testNet,
		Operator:   local,
		Treasuries: lookup{},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	got, err := r.Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeSponsor,
		Network: testNet,
		Account: operator.Address(),
		Tx:      payment(t, operator.Address()),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !got.Complete || len(got.Tx.Signatures()) != 1 {
		t.Fatalf("Complete = %v with %d signature(s), want true with 1",
			got.Complete, len(got.Tx.Signatures()))
	}

	if addr, err := r.OperatorAddress(context.Background()); err != nil || addr != operator.Address() {
		t.Fatalf("OperatorAddress = %q, %v; want %q", addr, err, operator.Address())
	}
}

func TestRouterRefusesAnEnvelopeForAnotherNetwork(t *testing.T) {
	operator := keyFor(t, 11)
	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	r, err := signer.NewRouter(signer.Config{
		Network:    testNet,
		Operator:   local,
		Treasuries: lookup{},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	_, err = r.Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeSponsor,
		Network: network.PublicNetworkPassphrase,
		Account: operator.Address(),
		Tx:      payment(t, operator.Address()),
	})
	if !errors.Is(err, signer.ErrNetworkMismatch) {
		t.Fatalf("got %v, want ErrNetworkMismatch", err)
	}
}

func TestRouterRefusesAChallengeWithNoWebAuthKey(t *testing.T) {
	operator := keyFor(t, 12)
	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	r, err := signer.NewRouter(signer.Config{
		Network:    testNet,
		Operator:   local,
		Treasuries: lookup{},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	_, err = r.Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeChallenge,
		Network: testNet,
		Account: operator.Address(),
		Tx:      payment(t, operator.Address()),
	})
	if !errors.Is(err, signer.ErrNoSigner) {
		t.Fatalf("got %v, want ErrNoSigner", err)
	}
}

func TestRequestValidation(t *testing.T) {
	operator := keyFor(t, 13)
	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	r, err := signer.NewRouter(signer.Config{
		Network:    testNet,
		Operator:   local,
		Treasuries: lookup{},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	tx := payment(t, operator.Address())
	bump, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner:      sign(t, tx, operator),
		FeeAccount: operator.Address(),
		BaseFee:    txnbuild.MinBaseFee * 2,
	})
	if err != nil {
		t.Fatalf("build fee bump: %v", err)
	}

	cases := map[string]signer.Request{
		"no purpose": {Network: testNet, Tx: tx},
		"no network": {Purpose: signer.PurposeSponsor, Tx: tx},
		"nothing":    {Purpose: signer.PurposeSponsor, Network: testNet},
		"both at once": {
			Purpose: signer.PurposeSponsor, Network: testNet, Tx: tx, FeeBump: bump,
		},
		"unknown purpose": {Purpose: "custody", Network: testNet, Tx: tx},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Sign(context.Background(), req); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestExternalRefusesAFeeBump(t *testing.T) {
	operator := keyFor(t, 14)
	tx := sign(t, payment(t, operator.Address()), operator)
	bump, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner:      tx,
		FeeAccount: operator.Address(),
		BaseFee:    txnbuild.MinBaseFee * 2,
	})
	if err != nil {
		t.Fatalf("build fee bump: %v", err)
	}

	_, err = signer.NewExternal(signer.Set{
		Signers: map[string]int32{operator.Address(): 1}, Medium: 1,
	}).Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: operator.Address(),
		FeeBump: bump,
	})
	if err == nil {
		t.Fatal("a treasury must never be asked to sign a fee bump")
	}
}

// TestAttributeNamesWhoSigned: the bot has to say who is still missing, not
// only how much weight is. A number alone leaves the channel guessing which
// three of five people to chase.
func TestAttributeNamesWhoSigned(t *testing.T) {
	a, b, c := keyFor(t, 30), keyFor(t, 31), keyFor(t, 32)
	stranger := keyFor(t, 33)

	set := signer.Set{
		Signers: map[string]int32{a.Address(): 1, b.Address(): 2, c.Address(): 3},
		Medium:  4,
	}
	tx := sign(t, payment(t, a.Address()), a, c, stranger)

	got, err := signer.Attribute(tx.Signatures(), tx, testNet, set)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("attributed %v, want two signers", got)
	}
	if got[a.Address()] != 1 || got[c.Address()] != 3 {
		t.Errorf("weights = %v", got)
	}
	if _, named := got[stranger.Address()]; named {
		t.Error("a signature from a key that is not on the account was attributed")
	}
	if _, named := got[b.Address()]; named {
		t.Error("a signer who did not sign was attributed")
	}
}
