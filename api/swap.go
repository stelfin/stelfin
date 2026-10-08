package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Swapping one asset a treasury holds for another, on the network's own
// exchange, as a proposal its signers sign.
//
// The treasury pays itself: a path payment whose destination is its source,
// sending a fixed amount of one asset and receiving at least a stated minimum
// of another. Nothing leaves the account but what the exchange takes. That
// minimum is the whole of the risk, and nothing here chooses it for anybody.

// SwapParams describes a swap to put to a treasury's signers.
type SwapParams struct {
	Treasury store.TreasuryID
	// Amount is what the treasury sends, exactly.
	Amount money.Stroops
	// From and To name the assets, as typed.
	From, To string
	// Minimum is the least the treasury will accept. Zero means it is to be
	// derived from the market and SlippageBps.
	Minimum money.Stroops
	// SlippageBps is how far below the quoted amount the treasury will go, in
	// hundredths of a percent, when Minimum is zero.
	SlippageBps int64
	CreatedBy   store.MemberID
}

// SwapQuote is what the market offers now.
type SwapQuote struct {
	From, To txnbuild.Asset
	// FromName and ToName are the assets as a reply names them.
	FromName, ToName string
	Send             money.Stroops
	// Receive is what the best path delivers at this moment. A reading, not a
	// promise.
	Receive money.Stroops
	// Path is the assets the exchange passes through, as a reply names them.
	// Empty means directly.
	Path  []string
	route []txnbuild.Asset
}

// MarketMovedError reports a swap that would fail if it were submitted now:
// the market no longer delivers the minimum its signers agreed to.
//
// Refused before submission, because submitting it is not harmless. A path
// payment that cannot meet its minimum fails on the ledger, which spends the
// treasury's sequence number, closes the proposal, and throws away every
// signature on it. Waiting costs nothing: the envelope and its signatures are
// good until the proposal's window closes.
type MarketMovedError struct {
	// Now is what the market delivers at the moment. Zero means nobody is
	// offering the exchange at all.
	Now, Minimum money.Stroops
	Asset        string
}

func (e *MarketMovedError) Error() string {
	return fmt.Sprintf("api: the market now delivers %s %s and this swap requires %s", e.Now, e.Asset, e.Minimum)
}

// quoteSwap resolves the assets against what the treasury holds and asks the
// market. It refuses a swap that could not land for a reason visible now.
func (s *Service) quoteSwap(
	ctx context.Context, treasury store.Treasury, amount money.Stroops, fromText, toText string,
) (*SwapQuote, error) {
	if amount.Sign() <= 0 {
		return nil, refusef("a swap has to send more than nothing")
	}
	state, err := s.accountState(ctx, treasury.Address)
	if err != nil {
		return nil, err
	}
	if !state.Exists {
		return nil, refusef("the treasury account does not exist on this network yet")
	}

	from, err := s.parseAsset(fromText, state.Holdings)
	if err != nil {
		return nil, err
	}
	to, err := s.parseAsset(toText, state.Holdings)
	if err != nil {
		return nil, err
	}
	if sameAsset(from, to) {
		return nil, refusef("that swaps %s for itself", s.AssetName(from))
	}

	// What it sends, it has to have — and to be free to send.
	held, has := state.holding(from)
	available := held.Balance - held.Selling
	if from.IsNative() {
		available = state.Spare
	}
	switch {
	case !has:
		return nil, refusef("the treasury holds no %s", s.AssetName(from))
	case !held.Authorized:
		return nil, refusef("the issuer has not authorised the treasury's %s, so it cannot be moved", s.AssetName(from))
	case available < amount:
		what := "holds"
		if from.IsNative() {
			what = "can spare"
		}
		return nil, refusef("the treasury %s %s %s, which is less than %s",
			what, available.Display(), s.AssetName(from), amount.Display())
	}

	// What it receives, it has to be able to hold.
	if !to.IsNative() {
		line, trusted := state.holding(to)
		switch {
		case !trusted:
			return nil, refusef("the treasury has no trustline for %s, so it could not receive it. "+
				"Open one first: /trust %s", s.AssetName(to), s.AssetName(to))
		case !line.Authorized:
			return nil, refusef("the issuer has not authorised the treasury to hold %s", s.AssetName(to))
		}
	}

	quote, err := s.settle.QuoteStrictSend(ctx, from, amount, to)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(quote.Path))
	for _, hop := range quote.Path {
		names = append(names, s.AssetName(hop))
	}
	return &SwapQuote{
		From: from, To: to, FromName: s.AssetName(from), ToName: s.AssetName(to),
		Send: amount, Receive: quote.Receive, Path: names, route: quote.Path,
	}, nil
}

// QuoteSwap reports what a swap would deliver now, and proposes nothing.
func (s *Service) QuoteSwap(
	ctx context.Context, scope Scope, id store.TreasuryID, amount money.Stroops, from, to string,
) (*SwapQuote, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	_, treasury, err := s.classicTreasury(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return s.quoteSwap(ctx, treasury, amount, from, to)
}

// ProposeSwap puts a swap to a treasury's signers.
//
// It returns the quote the proposal was built from and the minimum written
// into the envelope, so the caller can say both.
func (s *Service) ProposeSwap(
	ctx context.Context, scope Scope, p SwapParams,
) (*ProposalView, *SwapQuote, money.Stroops, error) {
	if err := scope.check(); err != nil {
		return nil, nil, 0, err
	}
	if err := s.writable(); err != nil {
		return nil, nil, 0, err
	}
	org, treasury, err := s.classicTreasury(ctx, scope, p.Treasury)
	if err != nil {
		return nil, nil, 0, err
	}
	quote, err := s.quoteSwap(ctx, treasury, p.Amount, p.From, p.To)
	if err != nil {
		return nil, nil, 0, err
	}

	// The minimum is the caller's. Either they named it, or they named how far
	// below today's market they will go. There is no third case with a number
	// chosen here.
	minimum := p.Minimum
	if minimum == 0 {
		if p.SlippageBps <= 0 {
			return nil, quote, 0, errors.New("api: a swap needs a minimum or a slippage tolerance")
		}
		if minimum, err = settlement.SlippageBound(quote.Receive, p.SlippageBps); err != nil {
			return nil, quote, 0, err
		}
	}
	if minimum.Sign() <= 0 {
		return nil, quote, 0, refusef("that leaves a minimum of nothing, which is no floor at all")
	}
	if minimum > quote.Receive {
		// It would fail on the ledger today, and take the signatures with it.
		return nil, quote, minimum, &MarketMovedError{
			Now: quote.Receive, Minimum: minimum, Asset: s.AssetName(quote.To),
		}
	}

	// A swap out of the deployment's asset moves that asset out of the
	// treasury, and counts against what the ceilings bound. A swap between
	// other assets is not something they measure.
	var spent money.Stroops
	if sameAsset(quote.From, s.cfg.Asset) {
		if err := s.checkSpend(ctx, scope.Org, p.Amount); err != nil {
			return nil, quote, minimum, err
		}
		spent = p.Amount
	}

	ops, err := settlement.Swap(settlement.SwapRequest{
		From: treasury.Address, To: treasury.Address,
		SendAsset: quote.From, DestAsset: quote.To,
		SendAmount: p.Amount, MinimumReceived: minimum,
		Path: quote.route,
	})
	if err != nil {
		return nil, quote, minimum, err
	}
	view, err := s.openProposal(ctx, scope, org, treasury, "dex", ops, "", spent, p.CreatedBy)
	return view, quote, minimum, err
}

// stillDelivers checks, just before a swap is submitted, that the market still
// meets the minimum its signers agreed to.
//
// A courtesy, and a consequential one. The network is the authority and would
// refuse the swap itself — by failing it on the ledger, which costs the
// proposal and every signature on it. Asking first turns that into "not yet".
// If the market cannot be asked, the swap goes ahead and the network decides:
// an answer that could not be had is not a reason to hold up a treasury.
func (s *Service) stillDelivers(ctx context.Context, tx *txnbuild.Transaction) error {
	ops := tx.Operations()
	if len(ops) != 1 {
		return nil
	}
	swap, ok := ops[0].(*txnbuild.PathPaymentStrictSend)
	if !ok {
		return nil
	}
	send, err := money.Parse(swap.SendAmount)
	if err != nil {
		return nil
	}
	minimum, err := money.Parse(swap.DestMin)
	if err != nil {
		return nil
	}

	quote, err := s.settle.QuoteStrictSend(ctx, swap.SendAsset, send, swap.DestAsset)
	switch {
	case errors.Is(err, settlement.ErrNoPath):
		return &MarketMovedError{Now: 0, Minimum: minimum, Asset: s.AssetName(swap.DestAsset)}
	case err != nil:
		return nil
	case quote.Receive < minimum:
		return &MarketMovedError{Now: quote.Receive, Minimum: minimum, Asset: s.AssetName(swap.DestAsset)}
	}
	return nil
}
