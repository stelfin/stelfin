package settlement

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// AccountSigners is an account's signer set as the network reports it now.
//
// Read at every point a decision depends on it, and never cached. The gap
// between a stored copy and the account is the window in which someone who has
// just been removed as a signer still counts as one, which is precisely the
// moment they would choose.
type AccountSigners struct {
	SignerSet

	// SkippedNonKeySigners counts signers that are not public keys.
	//
	// Hash(x) and pre-authorised-transaction signers carry real weight on the
	// account and neither can sign a challenge. Dropping them silently would
	// make a treasury look unprovable for no stated reason, so the count comes
	// back and the caller can explain it.
	SkippedNonKeySigners int
}

// Summary is the signer set in the shape SEP-10 threshold verification wants,
// master key included.
//
// Built here rather than at each call site: VerifyChallengeTxThreshold counts
// weight against this map, and a conversion that forgot the master key would
// report a perfectly good proof as insufficient.
func (a AccountSigners) Summary(account string) txnbuild.SignerSummary {
	out := make(txnbuild.SignerSummary, len(a.Signers)+1)
	for address, weight := range a.Signers {
		out[address] = int32(weight)
	}
	if a.MasterWeight > 0 {
		out[account] = int32(a.MasterWeight)
	}
	return out
}

// SignerSetOf reads an account's signers and thresholds from the network.
func (c *Client) SignerSetOf(ctx context.Context, address string) (AccountSigners, error) {
	if !strkey.IsValidEd25519PublicKey(address) {
		return AccountSigners{}, fmt.Errorf("settlement: %q is not a Stellar account", address)
	}

	account, err := c.LoadAccount(ctx, address)
	if err != nil {
		return AccountSigners{}, err
	}

	out := AccountSigners{SignerSet: SignerSet{
		Signers: make(map[string]uint32, len(account.Signers)),
		Low:     uint32(account.Thresholds.LowThreshold),
		Medium:  uint32(account.Thresholds.MedThreshold),
		High:    uint32(account.Thresholds.HighThreshold),
	}}

	for _, s := range account.Signers {
		// Weight zero means removed. Horizon does not usually list those, and
		// keeping one would make the bot name someone as a pending approver who
		// can no longer approve anything.
		if s.Weight <= 0 {
			continue
		}
		if !strkey.IsValidEd25519PublicKey(s.Key) {
			out.SkippedNonKeySigners++
			continue
		}
		// The master key arrives as an ordinary signer whose key is the account
		// itself. It is separated here because a signer change addresses it
		// through MasterWeight rather than through a Signer operation, and
		// conflating the two builds an envelope that locks the account.
		if s.Key == address {
			out.MasterWeight = uint32(s.Weight)
			continue
		}
		out.Signers[s.Key] = uint32(s.Weight)
	}
	return out, nil
}
