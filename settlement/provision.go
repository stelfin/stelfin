package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Provisioning creates a usable Stellar account for a user who holds no XLM
// and has no way to acquire any.
//
// A bare Stellar account needs 1 XLM of base reserve, plus 0.5 XLM per
// trustline, and cannot receive USDC at all until it trusts the issuer. A new
// WhatsApp user has none of that. CAP-33 sponsored reserves let the treasury
// carry the reserve while the user's account holds exactly zero XLM, and CAP-15
// fee-bumps let them transact without ever paying a fee.
//
// The four operations must be one atomic transaction. Splitting them produces
// half-provisioned accounts — created but untrusting, or trusting with nobody
// sponsoring the reserve — and every one of those becomes a support ticket.
//
//	BeginSponsoringFutureReserves   source: treasury, sponsored: user
//	CreateAccount                   source: treasury, starting balance 0
//	ChangeTrust                     source: user
//	EndSponsoringFutureReserves     source: user
//
// Both parties must sign: the treasury because it sources the first two
// operations and pays the fee, the user because they source the last two.
// Sponsorship is something the user accepts, not something done to them.

// ProvisionRequest describes the account to bring into existence.
type ProvisionRequest struct {
	// UserAddress is the account to create, as a G... public address.
	UserAddress string

	// Trustlines are the assets the account must be able to hold. USDC at
	// minimum; an account without its trustline cannot receive a cent.
	Trustlines []txnbuild.ChangeTrustAsset
}

// BuildProvision returns the unsigned sponsored-creation transaction.
//
// The treasury account is loaded from Horizon for its sequence number, so this
// call reaches the network even though it signs nothing.
func (c *Client) BuildProvision(
	ctx context.Context, treasuryAddress string, req ProvisionRequest,
) (*txnbuild.Transaction, error) {
	if req.UserAddress == "" {
		return nil, errors.New("settlement: user address is required")
	}
	if len(req.Trustlines) == 0 {
		return nil, errors.New(
			"settlement: at least one trustline is required; an account without one cannot receive anything")
	}

	treasury, err := c.LoadAccount(ctx, treasuryAddress)
	if err != nil {
		return nil, err
	}
	return c.buildProvisionOn(treasury, treasuryAddress, req)
}

// buildProvisionOn is the pure half of BuildProvision: given an account with a
// sequence number, it assembles the transaction without touching the network.
func (c *Client) buildProvisionOn(
	treasury txnbuild.Account, treasuryAddress string, req ProvisionRequest,
) (*txnbuild.Transaction, error) {
	ops := make([]txnbuild.Operation, 0, len(req.Trustlines)+3)

	ops = append(ops, &txnbuild.BeginSponsoringFutureReserves{
		SponsoredID:   req.UserAddress,
		SourceAccount: treasuryAddress,
	})

	// Starting balance is zero: the sponsorship covers the base reserve, and
	// handing the user XLM would defeat the point.
	ops = append(ops, &txnbuild.CreateAccount{
		Destination:   req.UserAddress,
		Amount:        "0",
		SourceAccount: treasuryAddress,
	})

	for _, line := range req.Trustlines {
		ops = append(ops, &txnbuild.ChangeTrust{
			Line:          line,
			Limit:         txnbuild.MaxTrustlineLimit,
			SourceAccount: req.UserAddress,
		})
	}

	ops = append(ops, &txnbuild.EndSponsoringFutureReserves{
		SourceAccount: req.UserAddress,
	})

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        treasury,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              c.baseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(int64(DefaultTimeout.Seconds())),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: build provisioning transaction for %s: %w", req.UserAddress, err)
	}
	return tx, nil
}

// FeeBump wraps a transaction so the treasury pays its fee.
//
// This is what lets a user with a zero XLM balance transact at all: they sign
// the inner transaction, and the treasury signs an envelope around it that
// carries the fee. The inner transaction is untouched, so the user's signature
// still commits to exactly what they approved.
func (c *Client) FeeBump(
	inner *txnbuild.Transaction, treasuryAddress string,
) (*txnbuild.FeeBumpTransaction, error) {
	// The fee-bump bid is per inner operation plus one for the bump itself, so
	// bidding the base fee here would underbid a multi-operation transaction.
	bump, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner:      inner,
		FeeAccount: treasuryAddress,
		BaseFee:    c.baseFee,
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: build fee-bump for %s: %w", treasuryAddress, err)
	}
	return bump, nil
}
