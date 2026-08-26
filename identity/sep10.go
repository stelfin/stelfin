// Package identity binds a chat account to a Stellar address, by signature.
//
// Three facts have to be connected and only one of them is provable.
//
// A chat account proves "the same account as last time" and nothing more. They
// are free to create, and handles are renameable and, once released,
// reassignable — which is why the transport identity stored elsewhere is the
// platform's immutable numeric id and never the handle.
//
// A Stellar address proves control of a key, and that is the fact everything
// else hangs off. The proof is a SEP-10 challenge: the server builds a
// transaction, the holder signs it, the server checks the signature.
//
// SEP-10 rather than a hand-rolled nonce for three reasons. It is Stellar's own
// standard for exactly this handshake, so a wallet that has met it before knows
// what it is being shown. The SDK already implements both halves, so the risky
// part is not being written here. And VerifyChallengeTxThreshold does the weight
// arithmetic, which turns out to be the primitive for the harder question:
// proving a *group* controls an M-of-N treasury means the same signatures a
// payment would need, not one signature from whoever typed the command.
//
// The challenge is built with sequence number zero. That makes it structurally
// unsubmittable — it can be signed and checked, and it can never move anything —
// which is what makes it safe to ask someone to sign it with the key that holds
// their money.
package identity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ChallengeTimeout is how long a challenge stays signable.
//
// Short. It is handed over in a chat message and completed in a browser tab
// that is already open; anything longer is a window with no purpose.
const ChallengeTimeout = 5 * time.Minute

var (
	// ErrInvalidAddress reports something that is not a Stellar address.
	ErrInvalidAddress = errors.New("identity: not a valid Stellar address")

	// ErrChallengeFailed reports a challenge that did not verify.
	ErrChallengeFailed = errors.New("identity: challenge signature does not verify")
)

// Config describes the challenge issuer.
type Config struct {
	// Seed is the web-auth account's secret.
	//
	// A distinct keypair from anything that moves money, and deliberately so.
	// It signs arbitrary attacker-chosen challenge material, and it controls
	// nothing: the account holds no balance and is never a source account.
	//
	// It is also the one key in this system that cannot go behind a remote
	// signer, because txnbuild.BuildChallengeTx takes a seed string rather than
	// a signing function. That is acceptable only because of what the account
	// is — which is why it must never be the operator's sponsor key.
	Seed string
	// HomeDomain and WebAuthDomain are the domains a signer is shown. Both are
	// checked on the way back in, so a challenge issued for one deployment
	// cannot be replayed against another.
	HomeDomain    string
	WebAuthDomain string
	// NetworkPassphrase binds the challenge to one network.
	NetworkPassphrase string
	// Timeout overrides ChallengeTimeout.
	Timeout time.Duration
}

// Challenges issues and verifies SEP-10 challenges.
type Challenges struct {
	seed          string
	serverAccount string
	homeDomain    string
	webAuthDomain string
	network       string
	timeout       time.Duration
}

// New returns a Challenges.
func New(cfg Config) (*Challenges, error) {
	kp, err := keypair.ParseFull(strings.TrimSpace(cfg.Seed))
	if err != nil {
		return nil, fmt.Errorf("identity: web-auth seed is not a valid Stellar secret: %w", err)
	}
	switch {
	case cfg.HomeDomain == "":
		return nil, errors.New("identity: home domain is required")
	case cfg.WebAuthDomain == "":
		return nil, errors.New("identity: web auth domain is required")
	case cfg.NetworkPassphrase == "":
		return nil, errors.New("identity: network passphrase is required")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = ChallengeTimeout
	}

	return &Challenges{
		seed:          kp.Seed(),
		serverAccount: kp.Address(),
		homeDomain:    cfg.HomeDomain,
		webAuthDomain: cfg.WebAuthDomain,
		network:       cfg.NetworkPassphrase,
		timeout:       timeout,
	}, nil
}

// ServerAccount is the address a signer will see as the challenge's source.
func (c *Challenges) ServerAccount() string { return c.serverAccount }

// Network is the passphrase challenges are built against.
func (c *Challenges) Network() string { return c.network }

// Challenge is an unsigned challenge, ready to hand to a wallet.
type Challenge struct {
	// Address is the account being asked to prove itself.
	Address string
	// XDR is the unsigned envelope.
	XDR string
	// Hash identifies it afterwards.
	Hash string
	// ExpiresAt mirrors the envelope's own time bounds.
	ExpiresAt time.Time
}

// Build issues a challenge for one address.
//
// Muxed (M...) and contract (C...) addresses are refused. Both have extra
// SEP-10 rules, and a contract account's authorisation is decided by contract
// code rather than by a signature — accepting one here would produce a proof
// this package cannot actually check.
func (c *Challenges) Build(address string) (*Challenge, error) {
	if !strkey.IsValidEd25519PublicKey(address) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAddress, address)
	}

	tx, err := txnbuild.BuildChallengeTx(
		c.seed, address, c.webAuthDomain, c.homeDomain, c.network, c.timeout, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: build challenge: %w", err)
	}

	xdr, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("identity: encode challenge: %w", err)
	}
	hash, err := tx.HashHex(c.network)
	if err != nil {
		return nil, fmt.Errorf("identity: hash challenge: %w", err)
	}

	return &Challenge{
		Address:   address,
		XDR:       xdr,
		Hash:      hash,
		ExpiresAt: time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	}, nil
}

// VerifyMember checks a returned challenge against a single-key address.
//
// The address is passed in rather than read out of what came back: otherwise a
// signer could return a challenge for an account they do control and have it
// accepted as proof of one they do not.
func (c *Challenges) VerifyMember(signedXDR, address string) error {
	if !strkey.IsValidEd25519PublicKey(address) {
		return fmt.Errorf("%w: %q", ErrInvalidAddress, address)
	}
	_, err := txnbuild.VerifyChallengeTxSigners(
		signedXDR, c.serverAccount, c.network, c.webAuthDomain,
		[]string{c.homeDomain}, address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrChallengeFailed, err)
	}
	return nil
}

// VerifyThreshold checks a returned challenge against an account's own
// thresholds, reporting which of its signers signed.
//
// For an M-of-N treasury this requires M signatures on the challenge — the same
// members who would have to approve a payment. That is a far stronger claim
// than one signature from whoever happened to type the command, and the SDK
// does the weight arithmetic.
//
// signers must come from the account as the network currently reports it, never
// from a cached copy: a stale signer set is exactly what someone being removed
// would exploit.
func (c *Challenges) VerifyThreshold(
	signedXDR string, threshold txnbuild.Threshold, signers txnbuild.SignerSummary,
) ([]string, error) {
	found, err := txnbuild.VerifyChallengeTxThreshold(
		signedXDR, c.serverAccount, c.network, c.webAuthDomain,
		[]string{c.homeDomain}, threshold, signers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChallengeFailed, err)
	}
	return found, nil
}

// Read parses a challenge and reports which account it was issued to, without
// checking any signature.
//
// Used to confirm that what came back is the challenge that went out, before
// anything is decided on the strength of it.
func (c *Challenges) Read(challengeXDR string) (address string, err error) {
	_, clientAccount, _, _, err := txnbuild.ReadChallengeTx(
		challengeXDR, c.serverAccount, c.network, c.webAuthDomain, []string{c.homeDomain})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrChallengeFailed, err)
	}
	return clientAccount, nil
}
