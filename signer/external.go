package signer

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// External is the signer that signs nothing.
//
// It is how this process handles a DAO's treasury, and it is not a placeholder
// or a stub — it is the correct implementation. The members hold the keys; what
// stelfin contributes is the envelope, the description and the arithmetic, and
// what it reports back is how much signing weight is still missing.
//
// The single-signer case is not special. It is External with a threshold of
// one, which is exactly right: a treasury with one signer is a treasury whose
// one member has not signed yet.
type External struct {
	set Set
}

// NewExternal returns a signer for an account this process cannot sign for.
func NewExternal(set Set) *External { return &External{set: set} }

// Address reports nothing to sign as.
//
// Deliberately an error rather than an empty string. A caller that needs an
// address is about to name a source account, and naming one this process cannot
// sign for would produce an envelope nobody can complete.
func (e *External) Address(context.Context) (string, error) {
	return "", errors.New("signer: this account is signed for elsewhere")
}

// Sign contributes nothing and reports what is still needed.
//
// Complete is false until the signatures already on the envelope reach the
// threshold. That is not a failure and must not be retried — the answer changes
// when a person signs, not when this is called again.
func (e *External) Sign(_ context.Context, req Request) (Response, error) {
	if req.FeeBump != nil {
		// A fee bump is the operator's envelope, not a treasury's. Routing one
		// here means a caller has the purposes mixed up, and signing it as if
		// the treasury were involved would be wrong in a way nothing later
		// would catch.
		return Response{}, errors.New(
			"signer: a fee bump is the operator's to sign, not a treasury's")
	}

	have, err := weightOf(req.Tx.Signatures(), req.Tx, req.Network, e.set)
	if err != nil {
		return Response{}, err
	}
	need := e.set.Medium

	return Response{
		Tx:       req.Tx,
		Complete: need > 0 && have >= need,
		Have:     have,
		Need:     need,
	}, nil
}

// Hasher is the part of an envelope the arithmetic below needs: the 32 bytes
// that were actually signed. Taken as an interface so a fee bump — whose hash
// covers a different structure entirely — can never be silently checked against
// a transaction's signers.
type Hasher interface {
	Hash(networkPassphrase string) ([32]byte, error)
}

// weightOf adds up the signing weight already on an envelope.
func weightOf(
	signatures []xdr.DecoratedSignature, env Hasher, networkPassphrase string, set Set,
) (int32, error) {
	signed, err := Attribute(signatures, env, networkPassphrase, set)
	if err != nil {
		return 0, err
	}
	var total int32
	for _, weight := range signed {
		total += weight
	}
	return total, nil
}

// Attribute reports which of the account's signers actually signed an envelope,
// and at what weight.
//
// Only signatures from keys currently on the account appear. A valid signature
// from a key that is not a signer contributes nothing, however well formed —
// which is the whole point of asking the account rather than counting
// signatures.
//
// Each signer appears at most once. Without that, an envelope carrying the same
// signature twice would report double the weight it has, and the bot would
// announce a proposal ready that the network will reject.
func Attribute(
	signatures []xdr.DecoratedSignature, env Hasher, networkPassphrase string, set Set,
) (map[string]int32, error) {
	if len(set.Signers) == 0 {
		return nil, errors.New("signer: the account has no signers; refusing to guess")
	}

	hash, err := env.Hash(networkPassphrase)
	if err != nil {
		return nil, fmt.Errorf("signer: hash envelope: %w", err)
	}

	// Parsed once per address rather than once per (address, signature) pair,
	// and parsed at all because a malformed address in the signer set is a
	// wrong answer waiting to happen: it would silently match nothing and the
	// weight would come out low for no visible reason.
	keys := make(map[string]*keypair.FromAddress, len(set.Signers))
	for address := range set.Signers {
		kp, err := keypair.ParseAddress(address)
		if err != nil {
			return nil, fmt.Errorf("signer: signer %q is not a valid account: %w", address, err)
		}
		keys[address] = kp
	}

	signed := make(map[string]int32)
	for _, sig := range signatures {
		for address, kp := range keys {
			if _, already := signed[address]; already {
				continue
			}
			// The hint is a cheap filter and nothing more — four bytes of the
			// public key, which collide. The verify below is the decision.
			if kp.Hint() != [4]byte(sig.Hint) {
				continue
			}
			if kp.Verify(hash[:], sig.Signature) != nil {
				continue
			}
			signed[address] = set.Signers[address]
			break
		}
	}
	return signed, nil
}
