package settlement

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Turning a simulated contract call into something signable.
//
// The SDK will build a Soroban transaction and will not stop you building a
// wrong one. Every rule here is one it does not enforce, and each is a way to
// lose money or authority that nothing downstream would catch.

// sorobanOp returns the transaction's single Soroban operation.
//
// One per transaction is a protocol rule, not a style choice: the footprint and
// the resource fee live on the transaction, not the operation, so two contract
// calls in one envelope cannot each carry their own. The SDK's builder silently
// keeps the last one it saw and drops the other's footprint, which produces an
// envelope that looks fine and fails on chain.
func sorobanOp(tx *txnbuild.Transaction) (txnbuild.SorobanOperation, error) {
	ops := tx.Operations()
	if len(ops) != 1 {
		return nil, fmt.Errorf("%w: %d operations, and Soroban allows one", ErrNotSoroban, len(ops))
	}
	op, ok := ops[0].(txnbuild.SorobanOperation)
	if !ok {
		return nil, fmt.Errorf("%w: it is a %T", ErrNotSoroban, ops[0])
	}
	return op, nil
}

// Assemble writes a simulation's result into the transaction, ready to sign.
//
// The footprint and the resource fee have to be on the envelope before anybody
// signs it. Signing first produces a signature over a transaction with no
// footprint — which the network rejects, after the person has already approved
// something.
func (c *Client) Assemble(
	tx *txnbuild.Transaction, sim *Simulation,
) (*txnbuild.Transaction, error) {
	if sim == nil {
		return nil, errors.New("settlement: cannot assemble without a simulation")
	}
	// Checked before anything else. Assembling changes the bytes a signature
	// covers, so a signature already on this envelope is about to become
	// worthless — and doing that silently leaves a caller holding approvals
	// that no longer approve anything.
	if len(tx.Signatures()) > 0 {
		return nil, errors.New(
			"settlement: this envelope is already signed; assembling changes what " +
				"the signature covers, so it must be assembled before it is signed")
	}
	op, err := sorobanOp(tx)
	if err != nil {
		return nil, err
	}

	var data xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionData, &data); err != nil {
		return nil, fmt.Errorf("settlement: decode simulation footprint: %w", err)
	}
	// Belt and braces against a mismatch between what Simulate checked and what
	// is about to be signed. The ceiling was applied to the number simulation
	// reported; this is the number that will actually be bid.
	if fee := int64(data.ResourceFee); fee > int64(MaxResourceFee) {
		return nil, fmt.Errorf("%w: the footprint bids %d stroops", ErrResourceFeeTooHigh, fee)
	}

	invoke, isInvoke := op.(*txnbuild.InvokeHostFunction)
	if isInvoke {
		// Never overwrite authorisation the caller supplied.
		//
		// Simulation records auth by pretending every signature it needs is
		// present. That recording is a convenience for the simple case where
		// the source account authorises its own call; it is NOT a statement
		// about who agreed to what. A transaction that already carries auth
		// carries entries somebody actually signed, and replacing them with
		// recorded ones would substitute "the network would accept this if
		// these people agreed" for "these people agreed".
		if len(invoke.Auth) == 0 && len(sim.Auth) > 0 {
			auth, authErr := decodeAuth(sim.Auth)
			if authErr != nil {
				return nil, authErr
			}
			invoke.Auth = auth
		}
		invoke.Ext = xdr.TransactionExt{V: 1, SorobanData: &data}
	} else {
		// ExtendFootprintTtl and RestoreFootprint carry their own Ext field,
		// and reaching it means naming the concrete type. Refused rather than
		// guessed at: an operation whose footprint never gets written is one
		// that fails on chain for a reason that looks like anything else.
		return nil, fmt.Errorf(
			"%w: assembling a %T is not implemented", ErrNotSoroban, op)
	}

	source := tx.SourceAccount()
	assembled, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		// The sequence is already the one this envelope reserves, taken from
		// the transaction being reassembled. Incrementing again would build for
		// a sequence one past the one simulation ran against, and the
		// simulation's footprint would be attached to a transaction that can
		// never be included.
		SourceAccount:        &source,
		IncrementSequenceNum: false,
		BaseFee:              c.baseFee,
		Memo:                 tx.Memo(),
		// Only the time bounds carry over. The SDK exposes no accessor for the
		// rest of the preconditions, and inventing empty ones would silently
		// drop a min-sequence or extra-signers condition somebody set — so the
		// builder above is the only supported way in, and anything richer has
		// to be assembled before it reaches here.
		Preconditions: txnbuild.Preconditions{TimeBounds: tx.Timebounds()},
		Operations:    []txnbuild.Operation{invoke},
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: assemble contract call: %w", err)
	}

	return assembled, nil
}

// decodeAuth turns simulation's recorded authorisation entries into XDR.
func decodeAuth(entries []string) ([]xdr.SorobanAuthorizationEntry, error) {
	out := make([]xdr.SorobanAuthorizationEntry, 0, len(entries))
	for i, encoded := range entries {
		var entry xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(encoded, &entry); err != nil {
			return nil, fmt.Errorf("settlement: decode authorisation entry %d: %w", i, err)
		}
		out = append(out, entry)
	}
	return out, nil
}
