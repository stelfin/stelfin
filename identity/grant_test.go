package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

func statementChallenges(t *testing.T) *Challenges {
	t.Helper()
	c, err := New(Config{
		Seed: keypair.MustRandom().Seed(), HomeDomain: "stelfin.test",
		WebAuthDomain: "stelfin.test", NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("challenges: %v", err)
	}
	return c
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func signedBy(t *testing.T, xdr string, keys ...*keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, _ := parsed.Transaction()
	tx, err = tx.Sign(network.TestNetworkPassphrase, keys...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := tx.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// TestAStatementTakesTheWeightThatSpends: what a signed statement proves is
// that the people who could move the money agreed to this text. One signature
// of the two a payment needs proves that one person did.
func TestAStatementTakesTheWeightThatSpends(t *testing.T) {
	c := statementChallenges(t)
	treasury, ada, ben := keypair.MustRandom(), keypair.MustRandom(), keypair.MustRandom()
	signers := txnbuild.SignerSummary{treasury.Address(): 1, ada.Address(): 1, ben.Address(): 1}
	digest := digestOf("the payroll sheet may be read")

	challenge, err := c.BuildStatement(treasury.Address(), digest)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}

	if _, err := c.VerifyStatement(signedBy(t, challenge.XDR, ada), 2, signers, digest); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("one signature of two: error = %v, want ErrChallengeFailed", err)
	}
	found, err := c.VerifyStatement(signedBy(t, challenge.XDR, ada, ben), 2, signers, digest)
	if err != nil {
		t.Fatalf("two signatures of two: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("signed by %v, want ada and ben", found)
	}

	// A stranger's signature is a valid signature from nobody.
	if _, err := c.VerifyStatement(signedBy(t, challenge.XDR, ada, keypair.MustRandom()), 2, signers, digest); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("a signer and a stranger: error = %v", err)
	}
}

// TestAStatementIsBoundToItsText is the reason the digest is in the envelope.
// Signatures over one statement are not agreement to another, and a challenge
// that proves control of an account is not agreement to anything at all.
func TestAStatementIsBoundToItsText(t *testing.T) {
	c := statementChallenges(t)
	treasury := keypair.MustRandom()
	signers := txnbuild.SignerSummary{treasury.Address(): 1}
	agreed, other := digestOf("read the payroll sheet"), digestOf("read everything")

	challenge, err := c.BuildStatement(treasury.Address(), agreed)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	signed := signedBy(t, challenge.XDR, treasury)

	if _, err := c.VerifyStatement(signed, 1, signers, agreed); err != nil {
		t.Fatalf("the statement that was signed: %v", err)
	}
	if _, err := c.VerifyStatement(signed, 1, signers, other); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("a different statement was accepted: %v", err)
	}

	// An ordinary treasury challenge, signed at full weight, says nothing.
	plain, err := c.Build(treasury.Address())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := c.VerifyStatement(signedBy(t, plain.XDR, treasury), 1, signers, agreed); !errors.Is(err, ErrChallengeFailed) {
		t.Fatalf("a plain challenge was accepted as a statement: %v", err)
	}
	// And the other way: a statement is still a good proof of control, which
	// is harmless, because proving control is all its signers did besides.
	if _, err := c.VerifyThreshold(signed, 1, signers); err != nil {
		t.Fatalf("a statement no longer verifies as a challenge: %v", err)
	}
}

// A statement challenge is as unsubmittable as any other: sequence zero, and
// nothing in it but data entries.
func TestAStatementChallengeCannotMoveAnything(t *testing.T) {
	c := statementChallenges(t)
	challenge, err := c.BuildStatement(keypair.MustRandom().Address(), digestOf("x"))
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	parsed, err := txnbuild.TransactionFromXDR(challenge.XDR)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, _ := parsed.Transaction()
	if tx.SequenceNumber() != 0 {
		t.Fatalf("sequence = %d, want 0", tx.SequenceNumber())
	}
	for _, op := range tx.Operations() {
		if _, ok := op.(*txnbuild.ManageData); !ok {
			t.Fatalf("a statement challenge contains a %T", op)
		}
	}

	for _, bad := range []string{"", "abc", digestOf("x")[:63] + "G", "ABCDEF" + digestOf("x")[6:]} {
		if _, err := c.BuildStatement(keypair.MustRandom().Address(), bad); err == nil {
			t.Errorf("%q was accepted as a digest", bad)
		}
	}
}
