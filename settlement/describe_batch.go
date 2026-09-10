package settlement

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// Describing a batch, which is a different question from describing a
// transaction.
//
// A general description answers "what does this do". A batch approval answers
// "how much am I about to spend, and to whom" — one number and a list — and
// somebody reading ninety rows will read the number and skim the list. That is
// not carelessness; it is what a total is for.
//
// Which means the total has to be true in a stronger sense than a rendered
// field usually is, and the rules below exist to make it so:
//
//   - every operation is a payment. One trustline change or one setOptions
//     hidden among ninety payments is invisible to a skimming reader, and the
//     total says nothing about it.
//   - one asset throughout. A total is only a number if the things added share
//     a unit; "90,000" across two assets is not wrong so much as meaningless.
//   - every operation is sourced by the same account. An operation with its own
//     source spends from somewhere the approver did not agree to, and the total
//     would still look right.
//
// Anything failing these is refused rather than rendered with a caveat. A
// caveat on a screen somebody is skimming is a caveat nobody reads.

// ErrNotABatch reports a transaction that must not be shown as one.
var ErrNotABatch = errors.New("settlement: this transaction cannot be shown as a batch")

// BatchDescription is what a batch approval screen is built from.
type BatchDescription struct {
	// Description is the general rendering, kept so the page can compare
	// canonical bytes exactly as every other screen does.
	Description *TxDescription

	// Payments are the rows, in envelope order.
	Payments []BatchPayment
	// Total is the exact sum, in the same asset as every row.
	Total money.Stroops
	// Asset is what is being sent, in the description's own vocabulary.
	Asset string
	// AssetCode is that asset's display code.
	AssetCode string
	// Source is the account every payment leaves.
	Source string
}

// BatchPayment is one row of a batch.
type BatchPayment struct {
	Index       int
	Amount      money.Stroops
	Destination string
}

// DescribeBatch renders a transaction as a batch, or refuses to.
//
// Deliberately a separate function from DescribeTx rather than a mode of it.
// The strictness here is not a display preference — it is what makes the total
// mean something — and a shared function with a flag is a function somebody
// eventually calls with the flag off.
func (c *Client) DescribeBatch(tx *txnbuild.Transaction) (*BatchDescription, error) {
	description, err := c.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	return AsBatch(description)
}

// AsBatch applies the batch rules to an existing description.
func AsBatch(d *TxDescription) (*BatchDescription, error) {
	if len(d.Operations) == 0 {
		return nil, fmt.Errorf("%w: it has no operations", ErrNotABatch)
	}

	out := &BatchDescription{Description: d, Source: d.Source}

	for _, op := range d.Operations {
		if op.Type != "payment" {
			// One trustline change hidden among ninety payments is invisible to
			// a reader checking a total.
			return nil, fmt.Errorf(
				"%w: operation %d is a %s, and a batch is payments only",
				ErrNotABatch, op.Index, op.Type)
		}
		if op.Source != d.Source {
			return nil, fmt.Errorf(
				"%w: operation %d is sent from %s rather than %s, so the total "+
					"would not describe what leaves either account",
				ErrNotABatch, op.Index, op.Source, d.Source)
		}

		amount, asset, destination := paymentFields(op)
		if asset == "" || destination == "" {
			return nil, fmt.Errorf("%w: operation %d is missing a field", ErrNotABatch, op.Index)
		}
		if out.Asset == "" {
			out.Asset = asset
			out.AssetCode = assetCode(asset)
		}
		if asset != out.Asset {
			// "90,000" across two assets is not wrong so much as meaningless.
			return nil, fmt.Errorf(
				"%w: operation %d sends %s and the batch sends %s. A total is only a "+
					"number when everything in it shares a unit",
				ErrNotABatch, op.Index, assetCode(asset), out.AssetCode)
		}

		parsed, err := money.Parse(amount)
		if err != nil {
			return nil, fmt.Errorf("%w: operation %d has an unreadable amount %q",
				ErrNotABatch, op.Index, amount)
		}
		if parsed.Sign() <= 0 {
			return nil, fmt.Errorf("%w: operation %d sends %s", ErrNotABatch, op.Index, parsed)
		}

		next := out.Total + parsed
		if next < out.Total {
			return nil, fmt.Errorf("%w: the total overflows", ErrNotABatch)
		}
		out.Total = next

		out.Payments = append(out.Payments, BatchPayment{
			Index: op.Index, Amount: parsed, Destination: destination,
		})
	}

	return out, nil
}

// TotalText is the total as the exact decimal a page compares against.
func (b *BatchDescription) TotalText() string { return b.Total.String() }

func paymentFields(op OpDescription) (amount, asset, destination string) {
	for _, f := range op.Fields {
		switch f.Label {
		case "amount":
			amount = f.Value
		case "asset":
			asset = f.Value
		case "destination":
			destination = f.Value
		}
	}
	return amount, asset, destination
}
