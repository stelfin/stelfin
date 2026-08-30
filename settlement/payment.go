package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// ErrIndescribable lives in describe.go now, alongside the renderer that raises
// it for every operation type rather than only for a payment.

// PaymentRequest describes a transfer to build.
type PaymentRequest struct {
	// From is the sending account. It sources the operation, so the user must
	// sign, and the treasury covers the fee with a fee-bump.
	From string
	// To is the receiving account.
	To string
	// Asset is what to send.
	Asset txnbuild.Asset
	// Amount is how much, in stroops.
	Amount money.Stroops
}

// BuildPayment returns an unsigned payment transaction sourced from the user.
//
// The user pays no fee. This is wrapped in a fee-bump before submission, which
// leaves the inner transaction — and therefore the user's signature — committed
// to exactly what they approved.
func (c *Client) BuildPayment(ctx context.Context, req PaymentRequest) (*txnbuild.Transaction, error) {
	if req.Amount.Sign() <= 0 {
		return nil, fmt.Errorf("settlement: payment amount %s is not positive", req.Amount)
	}
	if req.From == "" || req.To == "" {
		return nil, errors.New("settlement: payment needs both a source and a destination")
	}
	if req.From == req.To {
		return nil, errors.New("settlement: payment source and destination are the same account")
	}

	from, err := c.LoadAccount(ctx, req.From)
	if err != nil {
		return nil, err
	}
	return c.buildPaymentOn(from, req)
}

// buildPaymentOn is the pure half of BuildPayment.
func (c *Client) buildPaymentOn(from txnbuild.Account, req PaymentRequest) (*txnbuild.Transaction, error) {
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        from,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{
				Destination:   req.To,
				Amount:        req.Amount.String(),
				Asset:         req.Asset,
				SourceAccount: req.From,
			},
		},
		BaseFee:       c.baseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(int64(DefaultTimeout.Seconds()))},
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: build payment from %s: %w", req.From, err)
	}
	return tx, nil
}

// PaymentDescription is what a transaction will actually do, read back out of
// the transaction itself.
type PaymentDescription struct {
	From   string
	To     string
	Amount money.Stroops

	AssetCode   string
	AssetIssuer string
	AssetNative bool

	// Hash ties the description to the exact envelope being signed.
	Hash string
}
