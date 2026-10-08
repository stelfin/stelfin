package api

import (
	"context"
	"errors"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/mcp"
)

// AccountBalance is one account and what it holds now.
type AccountBalance struct {
	// Label is what the workspace calls it, when it calls it anything.
	Label   string
	Address string
	// Exists is false for an address nobody has funded: it has a key and no
	// account, which is an ordinary state for a wallet that was only linked.
	Exists bool
	// Contract marks a treasury held by a contract rather than by signatures.
	// A contract has no list of balances of its own, so its lines are only the
	// assets this deployment knows to ask about.
	Contract bool
	Lines    []BalanceLine
}

// BalanceLine is one asset in an account.
type BalanceLine struct {
	Asset  string
	Amount money.Stroops
	// Authorized is false when the issuer has frozen or not yet allowed the
	// line. The balance is there and cannot be moved.
	Authorized bool
}

// BalanceReport is what /balance shows.
type BalanceReport struct {
	// Treasuries is empty when the caller may not see them.
	Treasuries []AccountBalance
	// Wallets are the caller's own: one they linked, one stelfin set up.
	Wallets []AccountBalance
}

// Balances reads what the workspace's treasuries and the caller's own wallets
// hold, from the network, now.
//
// linked is the address the caller proved with /link, or "". The wallet stelfin
// set up for them is looked up by who they are in chat.
func (s *Service) Balances(
	ctx context.Context, scope Scope, treasuries bool, linked string,
) (*BalanceReport, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	report := &BalanceReport{}

	if treasuries {
		list, err := s.store.Treasuries(ctx, scope.Org)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if t.Kind != store.TreasuryClassic {
				balance, err := s.contractBalance(ctx, t)
				if err != nil {
					return nil, err
				}
				report.Treasuries = append(report.Treasuries, balance)
				continue
			}
			balance, err := s.balanceOf(ctx, t.Address, t.Label)
			if err != nil {
				return nil, err
			}
			report.Treasuries = append(report.Treasuries, balance)
		}
	}

	seen := map[string]bool{}
	add := func(address, label string) error {
		if address == "" || seen[address] {
			return nil
		}
		seen[address] = true
		balance, err := s.balanceOf(ctx, address, label)
		if err != nil {
			return err
		}
		report.Wallets = append(report.Wallets, balance)
		return nil
	}
	if err := add(linked, "linked"); err != nil {
		return nil, err
	}
	provisioned, err := s.store.AddressForOwner(ctx, scope.Org, scope.OwnerRef)
	switch {
	case errors.Is(err, store.ErrAddressNotTracked):
	case err != nil:
		return nil, err
	default:
		if err := add(provisioned, "set up by stelfin"); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func (s *Service) balanceOf(ctx context.Context, address, label string) (AccountBalance, error) {
	state, err := s.accountState(ctx, address)
	if err != nil {
		return AccountBalance{}, err
	}
	out := AccountBalance{Label: label, Address: address, Exists: state.Exists}
	for _, h := range state.Holdings {
		out.Lines = append(out.Lines, BalanceLine{
			Asset: s.AssetName(h.Asset), Amount: h.Balance, Authorized: h.Authorized,
		})
	}
	return out, nil
}

// TreasuryHoldings is what an agent is told a workspace's treasuries hold: the
// same reading /balance gives a member, from the network, now.
func (s *Service) TreasuryHoldings(ctx context.Context, org ledger.OrgID) ([]mcp.BalanceLine, error) {
	list, err := s.store.Treasuries(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []mcp.BalanceLine
	for _, t := range list {
		var balance AccountBalance
		if t.Kind == store.TreasuryClassic {
			balance, err = s.balanceOf(ctx, t.Address, t.Label)
		} else {
			balance, err = s.contractBalance(ctx, t)
		}
		if err != nil {
			return nil, err
		}
		name := t.Label
		if name == "" {
			name = t.Address
		}
		for _, line := range balance.Lines {
			// String, always: a JSON number is a float64 on the other side.
			out = append(out, mcp.BalanceLine{Account: name, Asset: line.Asset, Amount: line.Amount.String()})
		}
	}
	return out, nil
}
