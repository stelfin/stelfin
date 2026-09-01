package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/ledger/store"
)

// ErrTreasuryUnprovable reports an account whose control cannot be proved by
// signature at all.
//
// Distinct from a failed proof. An account with no key signers left — everything
// delegated to hash(x) or pre-authorised transactions — is not a treasury
// somebody signed for wrongly; it is one this handshake cannot ask about, and
// saying so is more use than "signature does not verify".
var ErrTreasuryUnprovable = errors.New("api: this account cannot prove control by signature")

// PrepareTreasuryLink issues a challenge proving a group controls a treasury.
//
// The same SEP-10 handshake as a member link, and a materially stronger claim:
// completing it needs the account's medium threshold, which is the weight a
// payment needs. One signature from whoever typed the command proves only that
// they are a signer, and on an M-of-N treasury that is a person who cannot move
// the money asking to be trusted with the org that can.
func (s *Service) PrepareTreasuryLink(
	ctx context.Context, scope Scope, identityID store.IdentityID, address string,
) (*LinkChallenge, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if identityID == 0 {
		return nil, errors.New("api: a challenge must be bound to a chat identity")
	}

	// Read before issuing, so an account that can never complete the handshake
	// is refused now rather than after somebody has gathered signatures for it.
	set, err := s.settle.SignerSetOf(ctx, address)
	if err != nil {
		return nil, err
	}
	if len(set.Summary(address)) == 0 {
		return nil, fmt.Errorf("%w: %s has no key signers", ErrTreasuryUnprovable, address)
	}
	if set.Medium > 0 && set.TotalWeight() < set.Medium {
		// Already locked out on chain: no set of signatures reaches the
		// threshold. Proving control is impossible, and so is spending.
		return nil, fmt.Errorf(
			"%w: %s needs weight %d to move money and its signers total %d",
			ErrTreasuryUnprovable, address, set.Medium, set.TotalWeight())
	}

	challenge, err := s.challenges.Build(address)
	if err != nil {
		return nil, err
	}

	if err := s.store.SaveChallenge(ctx, store.Challenge{
		Hash:      challenge.Hash,
		Org:       scope.Org,
		Identity:  identityID,
		Purpose:   store.PurposeLinkTreasury,
		Address:   challenge.Address,
		XDR:       challenge.XDR,
		ExpiresAt: challenge.ExpiresAt,
	}); err != nil {
		return nil, err
	}

	return &LinkChallenge{
		Address:           challenge.Address,
		XDR:               challenge.XDR,
		Hash:              challenge.Hash,
		NetworkPassphrase: s.challenges.Network(),
	}, nil
}

// TreasuryLinkResult describes a treasury whose control has been proved.
type TreasuryLinkResult struct {
	Treasury store.Treasury
	// Signed lists the account's signers that appeared on the challenge, so the
	// bot can say who proved it rather than only that somebody did.
	Signed []string
	// SkippedNonKeySigners is how many signers carried weight the handshake
	// cannot use. Reported so a treasury that needed more signatures than
	// expected has a stated reason.
	SkippedNonKeySigners int
}

// SubmitTreasuryLink checks a signed challenge against the account's own
// thresholds and records the treasury.
//
// The signer set is read from the network here and not reused from the one read
// when the challenge was issued. Between those two moments the account may have
// changed, and the only signer set worth checking against is the one that would
// authorise a payment now — a stale copy is exactly what someone being removed
// would exploit.
func (s *Service) SubmitTreasuryLink(
	ctx context.Context, scope Scope, hash, signedXDR, label string,
) (*TreasuryLinkResult, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if signedXDR == "" {
		return nil, errors.New("api: a signed challenge is required")
	}

	c, err := s.store.LiveChallenge(ctx, scope.Org, hash)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}
	if c.Purpose != store.PurposeLinkTreasury {
		// A member's challenge must not be redeemable as proof that a group
		// controls its money. The database refuses this too; refusing here as
		// well means the weaker check never runs at all.
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}

	set, err := s.settle.SignerSetOf(ctx, c.Address)
	if err != nil {
		return nil, err
	}
	summary := set.Summary(c.Address)
	if len(summary) == 0 {
		return nil, fmt.Errorf("%w: %s has no key signers", ErrTreasuryUnprovable, c.Address)
	}

	// Medium is what a payment needs, so it is what proving control means. Low
	// would accept a signer who cannot spend; high would refuse a group that
	// can.
	signed, err := s.challenges.VerifyThreshold(
		signedXDR, txnbuild.Threshold(set.Medium), summary)
	if err != nil {
		return nil, err
	}

	treasury, err := s.store.ConsumeTreasuryChallenge(ctx, hash, store.LinkTreasuryParams{
		Org:     scope.Org,
		Kind:    store.TreasuryClassic,
		Address: c.Address,
		Label:   label,
		Low:     int32(set.Low),
		Medium:  int32(set.Medium),
		High:    int32(set.High),
	})
	if errors.Is(err, store.ErrNoChallenge) {
		// Lost a race with another submission of the same signed challenge.
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}

	// The cache is written from the same read the proof was checked against, so
	// the two can never disagree about which account was just proved.
	cached := make(map[string]int32, len(summary))
	for address, weight := range summary {
		cached[address] = weight
	}
	if err := s.store.SaveSignerSet(ctx, scope.Org, treasury.ID, cached,
		int32(set.Low), int32(set.Medium), int32(set.High)); err != nil {
		return nil, err
	}

	return &TreasuryLinkResult{
		Treasury:             treasury,
		Signed:               signed,
		SkippedNonKeySigners: set.SkippedNonKeySigners,
	}, nil
}
