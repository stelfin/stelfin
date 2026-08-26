package identity

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

func newChallenges(t *testing.T) *Challenges {
	t.Helper()
	c, err := New(Config{
		Seed:              keypair.MustRandom().Seed(),
		HomeDomain:        "stelfin.example",
		WebAuthDomain:     "stelfin.example",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// sign returns the challenge signed by kp, as a wallet would.
func sign(t *testing.T, c *Challenges, ch *Challenge, kp ...*keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(ch.XDR)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("challenge is not a simple transaction")
	}
	signers := make([]*keypair.Full, len(kp))
	copy(signers, kp)
	signed, err := tx.Sign(c.Network(), signers...)
	if err != nil {
		t.Fatalf("sign challenge: %v", err)
	}
	xdr, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode signed challenge: %v", err)
	}
	return xdr
}

func TestNewValidatesConfig(t *testing.T) {
	valid := keypair.MustRandom().Seed()
	for name, cfg := range map[string]Config{
		"no seed":       {HomeDomain: "d", WebAuthDomain: "d", NetworkPassphrase: "n"},
		"public key":    {Seed: keypair.MustRandom().Address(), HomeDomain: "d", WebAuthDomain: "d", NetworkPassphrase: "n"},
		"no home":       {Seed: valid, WebAuthDomain: "d", NetworkPassphrase: "n"},
		"no web auth":   {Seed: valid, HomeDomain: "d", NetworkPassphrase: "n"},
		"no passphrase": {Seed: valid, HomeDomain: "d", WebAuthDomain: "d"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestChallengeRoundTrip(t *testing.T) {
	c := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := c.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ch.Address != kp.Address() {
		t.Errorf("address = %s", ch.Address)
	}
	if ch.Hash == "" || ch.XDR == "" {
		t.Fatal("challenge is missing its hash or envelope")
	}
	if !ch.ExpiresAt.After(time.Now()) {
		t.Errorf("expires at %v, which is not in the future", ch.ExpiresAt)
	}

	if err := c.VerifyMember(sign(t, c, ch, kp), kp.Address()); err != nil {
		t.Fatalf("VerifyMember: %v", err)
	}
}

// TestChallengeIsUnsubmittable is what makes it safe to ask someone to sign this
// with the key that holds their money.
//
// Sequence number zero can never be valid on chain: an account's next sequence
// is always at least one. So the thing being signed cannot move anything, no
// matter who ends up holding it afterwards.
func TestChallengeIsUnsubmittable(t *testing.T) {
	c := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := c.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	parsed, err := txnbuild.TransactionFromXDR(ch.XDR)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("not a simple transaction")
	}
	if seq := tx.SourceAccount().Sequence; seq != 0 {
		t.Fatalf("sequence = %d, want 0 — a challenge that could be submitted is "+
			"a transaction someone was tricked into signing", seq)
	}
}

// TestVerifyRejectsTheWrongKey: the address is supplied by the caller, not read
// out of what came back. Otherwise a signer could return a challenge for an
// account they do control and have it accepted as proof of one they do not.
func TestVerifyRejectsTheWrongKey(t *testing.T) {
	c := newChallenges(t)
	owner := keypair.MustRandom()
	impostor := keypair.MustRandom()

	ch, err := c.Build(owner.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Signed by someone else entirely.
	if err := c.VerifyMember(sign(t, c, ch, impostor), owner.Address()); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("error = %v, want ErrChallengeFailed", err)
	}
	// Signed correctly, but presented as proof of a different account.
	if err := c.VerifyMember(sign(t, c, ch, owner), impostor.Address()); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("error = %v, want ErrChallengeFailed", err)
	}
}

func TestVerifyRejectsAnUnsignedChallenge(t *testing.T) {
	c := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := c.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := c.VerifyMember(ch.XDR, kp.Address()); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("an unsigned challenge verified: %v", err)
	}
}

// TestChallengeIsBoundToItsDeployment: a challenge issued by one stelfin must
// not verify against another, or a signature collected by a hostile deployment
// could be replayed here.
func TestChallengeIsBoundToItsDeployment(t *testing.T) {
	mine := newChallenges(t)
	theirs := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := theirs.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	signed := sign(t, theirs, ch, kp)

	if err := mine.VerifyMember(signed, kp.Address()); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("another deployment's challenge verified here: %v", err)
	}
}

// TestChallengeIsBoundToItsNetwork: a testnet signature must not stand as proof
// on mainnet.
func TestChallengeIsBoundToItsNetwork(t *testing.T) {
	seed := keypair.MustRandom().Seed()
	testnet, err := New(Config{
		Seed: seed, HomeDomain: "d", WebAuthDomain: "d",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	public, err := New(Config{
		Seed: seed, HomeDomain: "d", WebAuthDomain: "d",
		NetworkPassphrase: network.PublicNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	kp := keypair.MustRandom()
	ch, err := testnet.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	signed := sign(t, testnet, ch, kp)

	if err := public.VerifyMember(signed, kp.Address()); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("a testnet signature verified against mainnet: %v", err)
	}
}

// TestBuildRefusesMuxedAndContractAddresses: both carry extra SEP-10 rules, and
// a contract account's authorisation is decided by contract code rather than by
// a signature — accepting one would produce a proof this package cannot check.
func TestBuildRefusesMuxedAndContractAddresses(t *testing.T) {
	c := newChallenges(t)
	for _, bad := range []string{
		"MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK",
		"CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE",
		"not-an-address",
		"",
	} {
		if _, err := c.Build(bad); !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("Build(%q) error = %v, want ErrInvalidAddress", bad, err)
		}
	}
}

// TestVerifyThresholdNeedsEnoughWeight is the primitive proving a group controls
// an M-of-N account: the same signatures a payment would need, not one from
// whoever happened to ask.
func TestVerifyThresholdNeedsEnoughWeight(t *testing.T) {
	c := newChallenges(t)
	master := keypair.MustRandom()
	second := keypair.MustRandom()

	ch, err := c.Build(master.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	signers := txnbuild.SignerSummary{
		master.Address(): 1,
		second.Address(): 1,
	}

	// One signature of weight 1 against a medium threshold of 2.
	if _, err := c.VerifyThreshold(sign(t, c, ch, master), 2, signers); err == nil {
		t.Fatal("one signature met a threshold of two")
	}

	found, err := c.VerifyThreshold(sign(t, c, ch, master, second), 2, signers)
	if err != nil {
		t.Fatalf("VerifyThreshold: %v", err)
	}
	if len(found) != 2 {
		t.Errorf("found %v, want both signers", found)
	}
}

// TestVerifyThresholdIgnoresUnknownSigners: a key that is not on the account
// contributes no weight, however valid its signature.
func TestVerifyThresholdIgnoresUnknownSigners(t *testing.T) {
	c := newChallenges(t)
	master := keypair.MustRandom()
	stranger := keypair.MustRandom()

	ch, err := c.Build(master.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	signers := txnbuild.SignerSummary{master.Address(): 1}

	if _, err := c.VerifyThreshold(sign(t, c, ch, master, stranger), 2, signers); err == nil {
		t.Fatal("a signature from a key not on the account counted toward the threshold")
	}
}

func TestReadReportsTheAccount(t *testing.T) {
	c := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := c.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := c.Read(ch.XDR)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != kp.Address() {
		t.Errorf("Read = %s, want %s", got, kp.Address())
	}

	if _, err := c.Read("not-xdr"); err == nil {
		t.Error("Read accepted something that is not a challenge")
	}
}

// TestWebAuthKeyIsNotAMoneyKey is a statement about deployment, checked where it
// can be: the challenge's source account is the web-auth account, so anything
// signing challenges is visible as such and must never be the operator's
// sponsor key.
func TestWebAuthKeyIsNotAMoneyKey(t *testing.T) {
	c := newChallenges(t)
	kp := keypair.MustRandom()

	ch, err := c.Build(kp.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	parsed, _ := txnbuild.TransactionFromXDR(ch.XDR)
	tx, _ := parsed.Transaction()

	if got := tx.SourceAccount().AccountID; got != c.ServerAccount() {
		t.Fatalf("challenge source = %s, want the web-auth account %s", got, c.ServerAccount())
	}
	if !strings.HasPrefix(c.ServerAccount(), "G") {
		t.Errorf("server account = %q", c.ServerAccount())
	}
}
