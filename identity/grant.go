package identity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// A statement, signed by a treasury.
//
// A SEP-10 challenge proves "the holders of these keys were here". This is the
// same envelope carrying one more fact: a digest. The signers of a treasury put
// their signatures on a challenge that names a statement by its hash, and what
// that proves is "the holders of these keys, at the weight that spends, agreed
// to exactly this text".
//
// It is how a workspace authorises something that is not a payment — a
// connector, to begin with — without a chat role standing in for a signature,
// and without a contract call. The envelope is as unsubmittable as any other
// challenge: sequence zero, and nothing in it but data entries.
//
// The digest rides in an extra manage-data operation sourced by the server
// account, which is where SEP-10 allows additional entries to go. A wallet that
// knows the standard shows it as a challenge with one more key in it.

// StatementKey is the manage-data entry a statement's digest is carried in.
const StatementKey = "stelfin statement"

// StatementTimeout is how long a statement challenge may collect signatures.
//
// Longer than a wallet link's, because it is collected from several people. It
// bounds the envelope and not the grant: once signed and recorded, a grant
// stands until it is revoked.
const StatementTimeout = time.Hour

// BuildStatement issues a challenge for address that commits to digest, the
// lower-case hex SHA-256 of whatever is being agreed to.
func (c *Challenges) BuildStatement(address, digest string) (*Challenge, error) {
	if !strkey.IsValidEd25519PublicKey(address) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAddress, address)
	}
	if !isDigest(digest) {
		return nil, fmt.Errorf("identity: %q is not a sha-256 digest", digest)
	}

	server, err := keypair.ParseFull(c.seed)
	if err != nil {
		return nil, fmt.Errorf("identity: web-auth seed: %w", err)
	}

	// 48 random bytes, base64 to 64 characters: the nonce SEP-10 specifies,
	// which is what makes two challenges for the same address different
	// transactions.
	nonce := make([]byte, 48)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("identity: generate nonce: %w", err)
	}

	now := time.Now().UTC()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		// Sequence zero, and not incremented. An account's next usable
		// sequence is at least one, so this can never be valid on chain
		// whoever ends up holding it.
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: c.serverAccount, Sequence: 0},
		IncrementSequenceNum: false,
		Operations: []txnbuild.Operation{
			&txnbuild.ManageData{
				SourceAccount: address,
				Name:          c.homeDomain + " auth",
				Value:         []byte(base64.StdEncoding.EncodeToString(nonce)),
			},
			&txnbuild.ManageData{
				SourceAccount: c.serverAccount,
				Name:          "web_auth_domain",
				Value:         []byte(c.webAuthDomain),
			},
			&txnbuild.ManageData{
				SourceAccount: c.serverAccount,
				Name:          StatementKey,
				// The digest as 64 hex characters, which is exactly what a
				// data entry holds, and which a person can compare by eye.
				Value: []byte(digest),
			},
		},
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimebounds(now.Unix(), now.Add(StatementTimeout).Unix()),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("identity: build statement challenge: %w", err)
	}
	tx, err = tx.Sign(c.network, server)
	if err != nil {
		return nil, fmt.Errorf("identity: sign statement challenge: %w", err)
	}

	xdr, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("identity: encode statement challenge: %w", err)
	}
	hash, err := tx.HashHex(c.network)
	if err != nil {
		return nil, fmt.Errorf("identity: hash statement challenge: %w", err)
	}
	return &Challenge{
		Address:   address,
		XDR:       xdr,
		Hash:      hash,
		ExpiresAt: time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	}, nil
}

// VerifyStatement checks a signed statement challenge against an account's own
// thresholds, and that it commits to digest. It reports which signers signed.
//
// Both halves matter and neither implies the other. The threshold check says
// the right people signed something; the digest check says what. An ordinary
// treasury challenge passes the first and must fail here, or a proof that a
// group controls an account could be replayed as that group's agreement to
// anything.
func (c *Challenges) VerifyStatement(
	signedXDR string, threshold txnbuild.Threshold, signers txnbuild.SignerSummary, digest string,
) ([]string, error) {
	if !isDigest(digest) {
		return nil, fmt.Errorf("identity: %q is not a sha-256 digest", digest)
	}
	found, err := c.VerifyThreshold(signedXDR, threshold, signers)
	if err != nil {
		return nil, err
	}

	parsed, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChallengeFailed, err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		return nil, fmt.Errorf("%w: not a plain transaction", ErrChallengeFailed)
	}

	carried := 0
	for _, op := range tx.Operations() {
		data, ok := op.(*txnbuild.ManageData)
		if !ok || data.Name != StatementKey {
			continue
		}
		carried++
		// Sourced by the server: an entry of this name sourced by anyone else
		// is not one this deployment put there.
		if data.SourceAccount != c.serverAccount || string(data.Value) != digest {
			return nil, fmt.Errorf("%w: it commits to a different statement", ErrChallengeFailed)
		}
	}
	if carried != 1 {
		return nil, fmt.Errorf("%w: it carries %d statements, want exactly one", ErrChallengeFailed, carried)
	}
	return found, nil
}

func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == s
}
