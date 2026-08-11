package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/ezedike-evan/stelfin/internal/money"
)

// ErrIndescribable reports a transaction this package will not summarise for a
// user. It is returned rather than a partial description on purpose: see
// Describe.
var ErrIndescribable = errors.New("settlement: transaction cannot be described")

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

// Describe reads a built transaction back and reports what it does.
//
// The confirmation shown to a user must be rendered from this, never from the
// PaymentRequest that produced the transaction. If the two ever diverge — a bug
// in the builder, a tampered request object, a future code path that mutates
// the transaction after building — then the user approves one thing while
// signing another, and their signature is on the wrong instruction. Deriving
// the display from the artifact under signature makes that divergence
// impossible rather than unlikely.
//
// Anything it cannot describe in full is refused. In particular a transaction
// carrying more than one operation is rejected: showing the user one payment
// while a second operation rides along in the same envelope is precisely the
// attack this guards against.
func (c *Client) Describe(tx *txnbuild.Transaction) (*PaymentDescription, error) {
	ops := tx.Operations()
	if len(ops) != 1 {
		return nil, fmt.Errorf("%w: %d operations, want exactly 1; a user cannot meaningfully "+
			"approve an envelope carrying more than the payment shown", ErrIndescribable, len(ops))
	}

	payment, ok := ops[0].(*txnbuild.Payment)
	if !ok {
		return nil, fmt.Errorf("%w: operation is %T, not a payment", ErrIndescribable, ops[0])
	}

	amount, err := money.Parse(payment.Amount)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable amount %q: %v", ErrIndescribable, payment.Amount, err)
	}
	if amount.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount %s is not positive", ErrIndescribable, amount)
	}

	hash, err := tx.HashHex(c.network)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIndescribable, err)
	}

	desc := &PaymentDescription{
		From:   payment.SourceAccount,
		To:     payment.Destination,
		Amount: amount,
		Hash:   hash,
	}
	if desc.From == "" {
		// No explicit operation source means the transaction's source pays.
		desc.From = tx.SourceAccount().AccountID
	}

	if payment.Asset == nil {
		return nil, fmt.Errorf("%w: payment has no asset", ErrIndescribable)
	}
	if payment.Asset.IsNative() {
		desc.AssetNative = true
		desc.AssetCode = "XLM"
		return desc, nil
	}

	desc.AssetCode = payment.Asset.GetCode()
	desc.AssetIssuer = payment.Asset.GetIssuer()
	if desc.AssetCode == "" || desc.AssetIssuer == "" {
		return nil, fmt.Errorf("%w: issued asset is missing its code or issuer", ErrIndescribable)
	}
	return desc, nil
}
