package api

import (
	"context"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Trustlines for a treasury, as proposals its signers sign.
//
// An account cannot hold an issued asset without a trustline to it, so this is
// the first thing a treasury needs before it can be paid in anything but XLM,
// and the first thing a swap into a new asset needs. Opening one is a
// transaction from the treasury like any other: it changes what the account
// will accept and locks half an XLM of reserve, and the people who decide that
// are the people who sign for it.

// classicTreasury loads a treasury that signs with keys.
func (s *Service) classicTreasury(
	ctx context.Context, scope Scope, id store.TreasuryID,
) (store.Org, store.Treasury, error) {
	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return store.Org{}, store.Treasury{}, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, id)
	if err != nil {
		return store.Org{}, store.Treasury{}, err
	}
	if treasury.Kind != store.TreasuryClassic {
		return store.Org{}, store.Treasury{}, refusef(
			"%s is held by a contract, and this is something an account's signers do", treasury.Address)
	}
	return org, treasury, nil
}

// ProposeTrust proposes that a treasury open a trustline to an asset.
func (s *Service) ProposeTrust(
	ctx context.Context, scope Scope, id store.TreasuryID, assetText string, by store.MemberID,
) (*ProposalView, string, error) {
	if err := scope.check(); err != nil {
		return nil, "", err
	}
	if err := s.writable(); err != nil {
		return nil, "", err
	}
	org, treasury, err := s.classicTreasury(ctx, scope, id)
	if err != nil {
		return nil, "", err
	}
	state, err := s.accountState(ctx, treasury.Address)
	if err != nil {
		return nil, "", err
	}
	if !state.Exists {
		return nil, "", refusef("the treasury account does not exist on this network yet")
	}

	asset, err := s.parseAsset(assetText, nil)
	if err != nil {
		return nil, "", err
	}
	credit, issued := asset.(txnbuild.CreditAsset)
	switch {
	case !issued:
		return nil, "", refusef("XLM needs no trustline: every account can hold it")
	case credit.Issuer == treasury.Address:
		return nil, "", refusef("the treasury issues %s itself, and an issuer needs no trustline to its own asset", credit.Code)
	}
	if _, has := state.holding(asset); has {
		return nil, "", refusef("the treasury already has a trustline for %s", s.AssetName(asset))
	}

	// The issuer has to exist. A trustline to an account nobody has created is
	// accepted by the network and is a line to an asset that cannot be issued,
	// which is what a mistyped issuer looks like from the inside.
	issuer, err := s.accountState(ctx, credit.Issuer)
	if err != nil {
		return nil, "", err
	}
	if !issuer.Exists {
		return nil, "", refusef("the issuer %s does not exist on this network, so nothing called %s "+
			"can come from it. Check the address", credit.Issuer, credit.Code)
	}

	// A trustline is something the account owns, and the network makes it keep
	// half an XLM for each such thing.
	if need := settlement.BaseReserve; state.Spare < need {
		return nil, "", refusef("the treasury has %s XLM to spare and a trustline locks %s. "+
			"It needs a little more XLM first", state.Spare.Display(), need.Display())
	}

	ops, err := settlement.Trust(settlement.TrustRequest{Account: treasury.Address, Asset: credit})
	if err != nil {
		return nil, "", err
	}
	view, err := s.openProposal(ctx, scope, org, treasury, "trustline", ops, "", 0, by)
	return view, s.AssetName(asset), err
}

// ProposeUntrust proposes that a treasury close a trustline.
func (s *Service) ProposeUntrust(
	ctx context.Context, scope Scope, id store.TreasuryID, assetText string, by store.MemberID,
) (*ProposalView, string, error) {
	if err := scope.check(); err != nil {
		return nil, "", err
	}
	if err := s.writable(); err != nil {
		return nil, "", err
	}
	org, treasury, err := s.classicTreasury(ctx, scope, id)
	if err != nil {
		return nil, "", err
	}
	state, err := s.accountState(ctx, treasury.Address)
	if err != nil {
		return nil, "", err
	}

	asset, err := s.parseAsset(assetText, state.Holdings)
	if err != nil {
		return nil, "", err
	}
	credit, issued := asset.(txnbuild.CreditAsset)
	if !issued {
		return nil, "", refusef("XLM has no trustline to close")
	}
	line, has := state.holding(asset)
	switch {
	case !has:
		return nil, "", refusef("the treasury has no trustline for %s", s.AssetName(asset))
	case line.Balance != 0:
		// The network refuses to close a line that still holds something, and
		// it refuses at submission, after everyone has signed.
		return nil, "", refusef("the treasury still holds %s %s. A trustline can only be closed "+
			"empty: pay or swap that away first", line.Balance.Display(), s.AssetName(asset))
	case line.Selling != 0:
		return nil, "", refusef("the treasury has offers open selling %s. They have to be "+
			"withdrawn before the trustline can be closed", s.AssetName(asset))
	}

	ops, err := settlement.Untrust(treasury.Address, credit)
	if err != nil {
		return nil, "", err
	}
	view, err := s.openProposal(ctx, scope, org, treasury, "trustline", ops, "", 0, by)
	return view, s.AssetName(asset), err
}
