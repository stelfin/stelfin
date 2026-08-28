package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

func newLinkTokensFor(t *testing.T) *LinkTokens {
	t.Helper()
	c, err := NewLinkTokens(testSecret)
	if err != nil {
		t.Fatalf("NewLinkTokens: %v", err)
	}
	return c
}

// TestTokenTypesAreNotInterchangeable is why there are three near-identical
// types rather than one generic one.
//
// They authorise different, non-overlapping things. The version is signed into
// the MAC, so even sharing a secret, none can be presented as another — a link
// token cannot become authority to submit a payment, and a confirm token cannot
// become authority to bind an address.
func TestTokenTypesAreNotInterchangeable(t *testing.T) {
	confirm := newTokens(t)
	enroll := newEnrollTokens(t)
	link := newLinkTokensFor(t)

	scope := Scope{Org: 7, OwnerRef: "telegram:1"}
	hash := strings.Repeat("a", 64)
	hour := time.Now().Add(time.Hour)

	confirmTok, err := confirm.Issue(scope, hash, hour)
	if err != nil {
		t.Fatalf("issue confirm: %v", err)
	}
	enrollTok, err := enroll.Issue(scope, hour)
	if err != nil {
		t.Fatalf("issue enroll: %v", err)
	}
	linkTok, err := link.Issue(scope, hash, hour)
	if err != nil {
		t.Fatalf("issue link: %v", err)
	}

	for name, check := range map[string]func() error{
		"link as confirm":   func() error { _, _, err := confirm.Verify(linkTok); return err },
		"enroll as confirm": func() error { _, _, err := confirm.Verify(enrollTok); return err },
		"confirm as link":   func() error { _, _, err := link.Verify(confirmTok); return err },
		"enroll as link":    func() error { _, _, err := link.Verify(enrollTok); return err },
		"confirm as enroll": func() error { _, err := enroll.Verify(confirmTok); return err },
		"link as enroll":    func() error { _, err := enroll.Verify(linkTok); return err },
	} {
		if err := check(); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
	}
}

func TestLinkTokenRoundTrip(t *testing.T) {
	c := newLinkTokensFor(t)
	scope := Scope{Org: 42, OwnerRef: "discord:9"}
	hash := strings.Repeat("b", 64)

	token, err := c.Issue(scope, hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	gotScope, gotHash, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotScope != scope || gotHash != hash {
		t.Errorf("verified %+v/%q, want %+v/%q", gotScope, gotHash, scope, hash)
	}
}

// linkFixture is a service with challenges configured, plus a member to bind to.
type linkFixture struct {
	svc      *Service
	store    *store.Store
	scope    Scope
	identity store.IdentityID
	kp       *keypair.Full
}

func newLinkFixture(t *testing.T) *linkFixture {
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

	settle, err := settlement.NewWith(&fakeHorizon{sequence: 1}, settlement.Config{
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
	org := orgFor(t, "/link")
	member, err := db.EnsureMember(ctx, org.ID, chat.Telegram, "u-link-"+org.Slug, "ada")
	if err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	id, err := db.IdentityFor(ctx, org.ID, chat.Telegram, "u-link-"+org.Slug)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = member

	return &linkFixture{
		svc: svc, store: db,
		scope:    Scope{Org: org.ID, OwnerRef: "telegram:u-link-" + org.Slug},
		identity: id,
		kp:       keypair.MustRandom(),
	}
}

// signChallenge signs a challenge the way a wallet would.
func signChallenge(t *testing.T, xdr string, kp *keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("challenge is not a simple transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestLinkRoundTrip(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()

	challenge, err := f.svc.PrepareLink(ctx, f.scope, f.identity, f.kp.Address())
	if err != nil {
		t.Fatalf("PrepareLink: %v", err)
	}

	// The page fetches it back by hash before signing.
	loaded, err := f.svc.LoadChallenge(ctx, f.scope, challenge.Hash)
	if err != nil {
		t.Fatalf("LoadChallenge: %v", err)
	}
	if loaded.XDR != challenge.XDR || loaded.Address != f.kp.Address() {
		t.Fatalf("loaded %+v", loaded)
	}

	res, err := f.svc.SubmitLink(ctx, f.scope, challenge.Hash, signChallenge(t, challenge.XDR, f.kp))
	if err != nil {
		t.Fatalf("SubmitLink: %v", err)
	}
	if res.Address != f.kp.Address() {
		t.Errorf("bound %s, want %s", res.Address, f.kp.Address())
	}

	member, err := f.store.Member(ctx, f.scope.Org, res.Member)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if member.Address != f.kp.Address() || member.AddressSource != store.AddressLinked {
		t.Errorf("member = %+v", member)
	}
}

// TestSubmitLinkRejectsTheWrongSigner: the signature is checked against the
// address the challenge was *issued* for, read from our own record — never from
// what came back. Otherwise a signer could return a challenge for an account
// they do control and have it accepted as proof of one they do not.
func TestSubmitLinkRejectsTheWrongSigner(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	impostor := keypair.MustRandom()

	challenge, err := f.svc.PrepareLink(ctx, f.scope, f.identity, f.kp.Address())
	if err != nil {
		t.Fatalf("PrepareLink: %v", err)
	}

	_, err = f.svc.SubmitLink(ctx, f.scope, challenge.Hash, signChallenge(t, challenge.XDR, impostor))
	if !errors.Is(err, identity.ErrChallengeFailed) {
		t.Fatalf("error = %v, want ErrChallengeFailed", err)
	}

	// And the challenge is still live: a failed attempt must not spend it, or a
	// wrong paste would cost someone their link.
	if _, err := f.svc.LoadChallenge(ctx, f.scope, challenge.Hash); err != nil {
		t.Errorf("a failed signature consumed the challenge: %v", err)
	}
}

// TestSubmitLinkIsSingleUse: replaying a valid signed challenge must not rebind.
func TestSubmitLinkIsSingleUse(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()

	challenge, err := f.svc.PrepareLink(ctx, f.scope, f.identity, f.kp.Address())
	if err != nil {
		t.Fatalf("PrepareLink: %v", err)
	}
	signed := signChallenge(t, challenge.XDR, f.kp)

	if _, err := f.svc.SubmitLink(ctx, f.scope, challenge.Hash, signed); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := f.svc.SubmitLink(ctx, f.scope, challenge.Hash, signed); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("second submit error = %v, want ErrNoChallenge", err)
	}
}

// TestLinkIsUnavailableWithoutAWebAuthKey: a deployment with none configured
// says so rather than failing obscurely.
func TestLinkIsUnavailableWithoutAWebAuthKey(t *testing.T) {
	f := newFixture(t, sendDecoded())
	ctx := context.Background()

	_, err := f.svc.PrepareLink(ctx, f.scope, 1, keypair.MustRandom().Address())
	if !errors.Is(err, ErrLinkingUnavailable) {
		t.Fatalf("error = %v, want ErrLinkingUnavailable", err)
	}
	if _, err := f.svc.LoadChallenge(ctx, f.scope, strings.Repeat("a", 64)); !errors.Is(err, ErrLinkingUnavailable) {
		t.Fatalf("error = %v, want ErrLinkingUnavailable", err)
	}
}

// TestPrepareLinkNeedsAnIdentity: a challenge bound to nothing could be
// completed by anyone who learned the address it names.
func TestPrepareLinkNeedsAnIdentity(t *testing.T) {
	f := newLinkFixture(t)
	if _, err := f.svc.PrepareLink(context.Background(), f.scope, 0, f.kp.Address()); err == nil {
		t.Fatal("a challenge was issued with no identity behind it")
	}
}
