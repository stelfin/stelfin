package settlement

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Changing who can sign for an account.
//
// This is the one operation in the system that can destroy a treasury with no
// recourse. Every other mistake is a payment to the wrong place — bad, and at
// least someone still holds the account. Lower the thresholds past what the
// remaining signers can reach and there is no transaction anyone can ever
// submit to fix it, because fixing it would itself need a signature that can no
// longer be produced.
//
// So the check is not a warning and it is not in the UI. It is here, and it
// makes an unsafe change impossible to *build*.

// ErrLockout reports a signer change that would leave an account nobody can
// sign for.
var ErrLockout = errors.New("settlement: this change would lock the account permanently")

// SignerChange adds, changes or removes one signer.
type SignerChange struct {
	Address string
	// Weight of zero removes the signer.
	Weight uint32
}

// MultisigRequest describes a change to an account's signers and thresholds.
type MultisigRequest struct {
	// Account is the account being changed. Its own signature is what
	// authorises this, so it sources the operations.
	Account string
	// Current is the account's signer set as the network reports it now.
	//
	// Required, and required to be *current*. The lockout check is arithmetic
	// over the resulting state, so it is only as good as its starting point —
	// a cached signer set is exactly what someone being removed would exploit.
	Current SignerSet
	// Changes are applied in order.
	Changes []SignerChange
	// MasterWeight, Low, Medium and High are set when non-nil.
	MasterWeight *uint32
	Low          *uint32
	Medium       *uint32
	High         *uint32
	// HomeDomain is set when non-nil.
	HomeDomain *string
}

// SignerSet is an account's signers and thresholds as the network reports them.
type SignerSet struct {
	// MasterWeight is the weight of the account's own key.
	MasterWeight uint32
	// Signers maps address to weight, excluding the master key.
	Signers map[string]uint32
	Low     uint32
	Medium  uint32
	High    uint32
}

// TotalWeight is every signature the account can currently produce, added up.
func (s SignerSet) TotalWeight() uint32 {
	total := s.MasterWeight
	for _, w := range s.Signers {
		total += w
	}
	return total
}

// Multisig returns the operations that apply a signer and threshold change.
//
// Signer changes come first and thresholds last, in one envelope. The order is
// load-bearing: lowering the master weight before the new signers exist leaves
// the account unable to authorise the rest of its own transaction, and if the
// envelope half-applies the account is gone.
func Multisig(req MultisigRequest) ([]txnbuild.Operation, error) {
	if !strkey.IsValidEd25519PublicKey(req.Account) {
		return nil, fmt.Errorf("settlement: %q is not a valid account", req.Account)
	}
	if req.Current.Signers == nil && req.Current.MasterWeight == 0 {
		return nil, errors.New(
			"settlement: a signer change needs the account's current signers; " +
				"the lockout check is arithmetic over the result and cannot be done blind")
	}

	resulting, err := applyChanges(req)
	if err != nil {
		return nil, err
	}
	if err := checkLockout(resulting); err != nil {
		return nil, err
	}

	var ops []txnbuild.Operation

	// One SetOptions per signer change. Stellar allows only one signer per
	// operation, so this is not a stylistic choice.
	for _, change := range req.Changes {
		if !strkey.IsValidEd25519PublicKey(change.Address) {
			return nil, fmt.Errorf("settlement: %q is not a valid signer", change.Address)
		}
		if change.Address == req.Account {
			return nil, errors.New(
				"settlement: the account's own key is changed with the master weight, " +
					"not by adding it as a signer")
		}
		weight := txnbuild.Threshold(change.Weight)
		ops = append(ops, &txnbuild.SetOptions{
			SourceAccount: req.Account,
			Signer:        &txnbuild.Signer{Address: change.Address, Weight: weight},
		})
	}

	// Thresholds and the master weight last, in a single operation, so the
	// account can still authorise everything above them.
	if req.MasterWeight != nil || req.Low != nil || req.Medium != nil ||
		req.High != nil || req.HomeDomain != nil {
		op := &txnbuild.SetOptions{SourceAccount: req.Account, HomeDomain: req.HomeDomain}
		if req.MasterWeight != nil {
			t := txnbuild.Threshold(*req.MasterWeight)
			op.MasterWeight = &t
		}
		if req.Low != nil {
			t := txnbuild.Threshold(*req.Low)
			op.LowThreshold = &t
		}
		if req.Medium != nil {
			t := txnbuild.Threshold(*req.Medium)
			op.MediumThreshold = &t
		}
		if req.High != nil {
			t := txnbuild.Threshold(*req.High)
			op.HighThreshold = &t
		}
		ops = append(ops, op)
	}

	if len(ops) == 0 {
		return nil, errors.New("settlement: this signer change changes nothing")
	}
	return ops, nil
}

// applyChanges computes the signer set this request would produce.
func applyChanges(req MultisigRequest) (SignerSet, error) {
	out := SignerSet{
		MasterWeight: req.Current.MasterWeight,
		Signers:      make(map[string]uint32, len(req.Current.Signers)+len(req.Changes)),
		Low:          req.Current.Low,
		Medium:       req.Current.Medium,
		High:         req.Current.High,
	}
	for address, weight := range req.Current.Signers {
		out.Signers[address] = weight
	}

	for _, change := range req.Changes {
		if change.Weight > 255 {
			return SignerSet{}, fmt.Errorf(
				"settlement: signer weight %d is above the maximum of 255", change.Weight)
		}
		if change.Weight == 0 {
			delete(out.Signers, change.Address)
			continue
		}
		out.Signers[change.Address] = change.Weight
	}

	for name, value := range map[string]*uint32{
		"master weight": req.MasterWeight, "low": req.Low,
		"medium": req.Medium, "high": req.High,
	} {
		if value != nil && *value > 255 {
			return SignerSet{}, fmt.Errorf(
				"settlement: %s threshold %d is above the maximum of 255", name, *value)
		}
	}

	if req.MasterWeight != nil {
		out.MasterWeight = *req.MasterWeight
	}
	if req.Low != nil {
		out.Low = *req.Low
	}
	if req.Medium != nil {
		out.Medium = *req.Medium
	}
	if req.High != nil {
		out.High = *req.High
	}
	return out, nil
}

// checkLockout refuses a signer set nobody can satisfy.
//
// Two conditions, and both are about the same thing: after this change, can the
// account still authorise a transaction — including the transaction that would
// undo this change?
//
//   - The signatures available must reach the high threshold. High is what a
//     further signer change needs, so an account that cannot reach it can never
//     be repaired.
//   - The thresholds must not be inverted. A low above a medium is not
//     immediately fatal, but it means a payment needs more signatures than a
//     signer change, which is not what anyone intends and is a reliable sign
//     the caller has the arguments in the wrong order.
func checkLockout(s SignerSet) error {
	total := s.TotalWeight()
	if total == 0 {
		return fmt.Errorf("%w: no signer would be left", ErrLockout)
	}
	if s.High > total {
		return fmt.Errorf(
			"%w: the high threshold would be %d but only %d of signing weight would remain, "+
				"so no future signer change could ever be authorised",
			ErrLockout, s.High, total)
	}
	if s.Medium > total || s.Low > total {
		return fmt.Errorf(
			"%w: a threshold would be above the %d of signing weight that would remain",
			ErrLockout, total)
	}
	if s.Low > s.Medium || s.Medium > s.High {
		return fmt.Errorf(
			"%w: thresholds would be out of order (low %d, medium %d, high %d); "+
				"a payment would need more signatures than a signer change",
			ErrLockout, s.Low, s.Medium, s.High)
	}
	return nil
}
