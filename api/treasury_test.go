package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// signerHorizon reports whatever signer set a test sets, and lets a test change
// it between issuing a challenge and completing one.
type signerHorizon struct {
	fakeHorizon
	signers    []horizon.Signer
	thresholds horizon.AccountThresholds
	balances   []horizon.Balance
	data       map[string]string
	sponsoring uint32
	submitted  []*txnbuild.Transaction
}

// SubmitTransactionWithOptions answers with the envelope's real hash. The
// shared fake returns a fixed string, which is fine where nothing reads it and
// wrong here: the hash is written to a column that checks it is 64 hex
// characters, and a test that never hits that check would not be testing the
// submit path.
func (h *signerHorizon) SubmitTransactionWithOptions(
	tx *txnbuild.Transaction, _ horizonclient.SubmitTxOpts,
) (horizon.Transaction, error) {
	hash, err := tx.HashHex(network.TestNetworkPassphrase)
	if err != nil {
		return horizon.Transaction{}, err
	}
	h.submitted = append(h.submitted, tx)
	return horizon.Transaction{
		Hash: hash, Ledger: 42, LedgerCloseTime: time.Unix(1700000000, 0),
	}, nil
}

func (h *signerHorizon) AccountDetail(req horizonclient.AccountRequest) (horizon.Account, error) {
	seq := h.fakeHorizon.sequence
	if seq == 0 {
		seq = 1
	}
	return horizon.Account{
		AccountID:     req.AccountID,
		Sequence:      seq,
		Signers:       h.signers,
		Thresholds:    h.thresholds,
		Balances:      h.balances,
		Data:          h.data,
		NumSponsoring: h.sponsoring,
	}, nil
}

// treasuryFixture is a service whose Horizon reports a treasury's signers.
type treasuryFixture struct {
	svc      *Service
	store    *store.Store
	horizon  *signerHorizon
	scope    Scope
	identity store.IdentityID
	user     string
	treasury *keypair.Full
}

func newTreasuryFixture(t *testing.T, name string) *treasuryFixture {
	t.Helper()
	ctx := context.Background()

	challenges, err := identity.New(identity.Config{
		Seed:              keypair.MustRandom().Seed(),
		HomeDomain:        "stelfin.test",
		WebAuthDomain:     "stelfin.test",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("challenges: %v", err)
	}

	h := &signerHorizon{}
	settle, err := settlement.NewWith(h, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}

	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{
			Asset:      txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer},
			AssetCode:  "USDC",
			Challenges: challenges,
		})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	db := store.New(testPool)
	org := orgFor(t, name)
	user := "u-" + org.Slug
	if _, err := db.EnsureMember(ctx, org.ID, chat.Telegram, user, "ada"); err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	id, err := db.IdentityFor(ctx, org.ID, chat.Telegram, user)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	return &treasuryFixture{
		svc: svc, store: db, horizon: h,
		scope:    Scope{Org: org.ID, OwnerRef: "telegram:" + user},
		identity: id,
		user:     user,
		treasury: keypair.MustRandom(),
	}
}

// twoOfTwo puts the treasury's own key and one co-signer on the account, each
// at weight 1, with every threshold at 2.
func (f *treasuryFixture) twoOfTwo(co *keypair.Full) {
	f.horizon.signers = []horizon.Signer{
		{Key: f.treasury.Address(), Weight: 1, Type: "ed25519_public_key"},
		{Key: co.Address(), Weight: 1, Type: "ed25519_public_key"},
	}
	f.horizon.thresholds = horizon.AccountThresholds{
		LowThreshold: 2, MedThreshold: 2, HighThreshold: 2,
	}
}

// twoOfTwoWithout removes a co-signer from the account, leaving the treasury's
// own key at weight 1 against a threshold of 2.
func (f *treasuryFixture) twoOfTwoWithout(_ *keypair.Full) {
	f.horizon.signers = []horizon.Signer{
		{Key: f.treasury.Address(), Weight: 1, Type: "ed25519_public_key"},
	}
	f.horizon.thresholds = horizon.AccountThresholds{
		LowThreshold: 2, MedThreshold: 2, HighThreshold: 2,
	}
}

func signChallengeWith(t *testing.T, xdr string, kps ...*keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("challenge is not a simple transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, kps...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// TestTreasuryLinkNeedsTheWeightAPaymentNeeds is the whole point of the
// command. One signature from whoever typed it proves only that they are a
// signer; on a 2-of-2 treasury that is a person who cannot move the money
// asking to be trusted with the org that can.
func TestTreasuryLinkNeedsTheWeightAPaymentNeeds(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury weight")
	ctx := context.Background()
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	challenge, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}

	// One of two is a real signature and not enough.
	_, err = f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash,
		signChallengeWith(t, challenge.XDR, f.treasury), "main")
	if !errors.Is(err, identity.ErrChallengeFailed) {
		t.Fatalf("one of two signatures: %v, want ErrChallengeFailed", err)
	}

	// Both is.
	res, err := f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash,
		signChallengeWith(t, challenge.XDR, f.treasury, co), "main")
	if err != nil {
		t.Fatalf("SubmitTreasuryLink: %v", err)
	}
	if res.Treasury.Address != f.treasury.Address() || res.Treasury.Medium != 2 {
		t.Fatalf("linked %+v", res.Treasury)
	}
	if len(res.Signed) != 2 {
		t.Errorf("Signed = %v, want both signers", res.Signed)
	}

	// The cache is written from the same read the proof was checked against.
	cached, err := f.store.CachedSignerSet(ctx, f.scope.Org, res.Treasury.ID)
	if err != nil {
		t.Fatalf("cached signer set: %v", err)
	}
	if len(cached) != 2 || cached[co.Address()] != 1 || cached[f.treasury.Address()] != 1 {
		t.Errorf("cached = %v", cached)
	}
}

// TestTreasuryLinkRereadsTheSignerSet: the account may change between issuing a
// challenge and completing one, and the only signer set worth checking against
// is the one that would authorise a payment now.
func TestTreasuryLinkRereadsTheSignerSet(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury reread")
	ctx := context.Background()
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	challenge, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}
	signed := signChallengeWith(t, challenge.XDR, f.treasury, co)

	// The co-signer is removed on chain and a third party is added, raising the
	// threshold. The two signatures gathered a moment ago no longer authorise
	// anything.
	third := keypair.MustRandom()
	f.horizon.signers = []horizon.Signer{
		{Key: f.treasury.Address(), Weight: 1, Type: "ed25519_public_key"},
		{Key: third.Address(), Weight: 1, Type: "ed25519_public_key"},
	}
	f.horizon.thresholds = horizon.AccountThresholds{
		LowThreshold: 2, MedThreshold: 2, HighThreshold: 2,
	}

	if _, err := f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash, signed, "main"); !errors.Is(
		err, identity.ErrChallengeFailed,
	) {
		t.Fatalf("a signer set read before the change was accepted: %v", err)
	}
}

// TestMemberChallengeIsNotATreasuryProof: the two purposes share a table, and a
// challenge issued to prove one person's wallet must not be redeemable as proof
// that a group controls its money.
func TestMemberChallengeIsNotATreasuryProof(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury purpose")
	ctx := context.Background()
	f.horizon.signers = []horizon.Signer{
		{Key: f.treasury.Address(), Weight: 1, Type: "ed25519_public_key"},
	}
	f.horizon.thresholds = horizon.AccountThresholds{MedThreshold: 1}

	challenge, err := f.svc.PrepareLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareLink: %v", err)
	}

	_, err = f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash,
		signChallengeWith(t, challenge.XDR, f.treasury), "main")
	if !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a member challenge linked a treasury: %v", err)
	}
}

// TestUnprovableAccountsAreRefusedBeforeAnyoneSigns: an account whose weight
// cannot reach its own medium threshold, or which has no key signers at all,
// cannot prove control and cannot spend either. Saying so now is more use than
// "signature does not verify" after a group has gathered signatures.
func TestUnprovableAccountsAreRefusedBeforeAnyoneSigns(t *testing.T) {
	ctx := context.Background()

	t.Run("locked out on chain", func(t *testing.T) {
		f := newTreasuryFixture(t, "/link-treasury lockout")
		f.horizon.signers = []horizon.Signer{
			{Key: f.treasury.Address(), Weight: 1, Type: "ed25519_public_key"},
		}
		f.horizon.thresholds = horizon.AccountThresholds{MedThreshold: 5}

		_, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
		if !errors.Is(err, ErrTreasuryUnprovable) {
			t.Fatalf("got %v, want ErrTreasuryUnprovable", err)
		}
	})

	t.Run("no key signers", func(t *testing.T) {
		f := newTreasuryFixture(t, "/link-treasury nokeys")
		f.horizon.signers = []horizon.Signer{
			{
				Key:    "XDRPF6NZRR7EEVO7ESIWUDXHAOMM2QSKIQQBJK6I2FB7YKDZES5UCLWD",
				Weight: 5, Type: "sha256_hash",
			},
		}
		f.horizon.thresholds = horizon.AccountThresholds{MedThreshold: 1}

		_, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
		if !errors.Is(err, ErrTreasuryUnprovable) {
			t.Fatalf("got %v, want ErrTreasuryUnprovable", err)
		}
	})
}

// TestTreasuryProofIsSpentOnce: a signed challenge is a bearer proof, and
// replaying one must not relink anything.
func TestTreasuryProofIsSpentOnce(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury replay")
	ctx := context.Background()
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	challenge, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}
	signed := signChallengeWith(t, challenge.XDR, f.treasury, co)

	if _, err := f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash, signed, "main"); err != nil {
		t.Fatalf("SubmitTreasuryLink: %v", err)
	}
	if _, err := f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash, signed, "main"); !errors.Is(
		err, ErrNoChallenge,
	) {
		t.Fatalf("replay: %v, want ErrNoChallenge", err)
	}
}

// TestTreasuryChallengeIsNotAWalletProof is the mirror of the test above, and
// the more dangerous direction. Redeeming a treasury challenge as a member link
// would spend the group's proof and bind the treasury's address to whoever
// presented it, as if it were their personal wallet.
func TestTreasuryChallengeIsNotAWalletProof(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury not-a-wallet")
	ctx := context.Background()
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	challenge, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}

	_, err = f.svc.SubmitLink(ctx, f.scope, challenge.Hash,
		signChallengeWith(t, challenge.XDR, f.treasury, co))
	if !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a treasury challenge bound a member's wallet: %v", err)
	}

	// And it is still live for what it was issued for.
	if _, err := f.svc.SubmitTreasuryLink(ctx, f.scope, challenge.Hash,
		signChallengeWith(t, challenge.XDR, f.treasury, co), "main"); err != nil {
		t.Fatalf("the treasury could no longer complete it: %v", err)
	}
}

// TestSubmitChallengeDispatchesOnTheStoredPurpose: the purpose comes from our
// own record, so a client cannot choose which verification runs against its
// signature.
func TestSubmitChallengeDispatchesOnTheStoredPurpose(t *testing.T) {
	f := newTreasuryFixture(t, "/link-treasury dispatch")
	ctx := context.Background()
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	treasuryChallenge, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}
	done, err := f.svc.SubmitChallenge(ctx, f.scope, treasuryChallenge.Hash,
		signChallengeWith(t, treasuryChallenge.XDR, f.treasury, co), "main")
	if err != nil {
		t.Fatalf("SubmitChallenge: %v", err)
	}
	if done.Treasury == nil || done.Member != nil {
		t.Fatalf("dispatched to %+v", done)
	}

	wallet := keypair.MustRandom()
	memberChallenge, err := f.svc.PrepareLink(ctx, f.scope, f.identity, wallet.Address())
	if err != nil {
		t.Fatalf("PrepareLink: %v", err)
	}
	done, err = f.svc.SubmitChallenge(ctx, f.scope, memberChallenge.Hash,
		signChallengeWith(t, memberChallenge.XDR, wallet), "")
	if err != nil {
		t.Fatalf("SubmitChallenge: %v", err)
	}
	if done.Member == nil || done.Treasury != nil {
		t.Fatalf("dispatched to %+v", done)
	}
	if done.Member.Address != wallet.Address() {
		t.Errorf("bound %s", done.Member.Address)
	}
}
