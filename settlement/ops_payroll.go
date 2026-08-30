package settlement

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// Paying several people at once.
//
// One transaction, so a batch either lands entirely or not at all — which is
// what a payroll run needs, and what a loop of single payments cannot give.
//
// Up to a point. Stellar caps a transaction at 100 operations, so a batch above
// that becomes several transactions and stops being atomic. That is stated
// loudly rather than hidden, because "half the team got paid" is a situation
// somebody has to resolve by hand and they need to know it can happen.

// PayrollChunkSize is how many payments go in one transaction.
//
// Below the protocol's 100 to leave headroom: a batch may travel with a
// trustline change or a sponsorship wrapper in the same envelope, and a caller
// that filled the transaction exactly would find those had nowhere to go.
const PayrollChunkSize = 95

// PayrollLine is one payment in a batch.
type PayrollLine struct {
	To     string
	Asset  txnbuild.Asset
	Amount money.Stroops
}

// Payroll returns the operations paying every line from one account.
//
// Every operation names its source explicitly. Without it an operation inherits
// the transaction's source, which is right until the day a batch is wrapped in
// a fee bump or built on a channel account — and then the payments would come
// from whichever account happened to be paying the fee.
func Payroll(from string, lines []PayrollLine) ([]txnbuild.Operation, error) {
	if !strkey.IsValidEd25519PublicKey(from) {
		return nil, fmt.Errorf("settlement: %q is not a valid sending account", from)
	}
	if len(lines) == 0 {
		return nil, errors.New("settlement: a payroll batch needs at least one line")
	}
	if len(lines) > PayrollChunkSize {
		return nil, fmt.Errorf(
			"settlement: %d lines is more than one transaction holds; split with SplitPayroll",
			len(lines))
	}

	ops := make([]txnbuild.Operation, 0, len(lines))
	for i, line := range lines {
		switch {
		case !strkey.IsValidEd25519PublicKey(line.To):
			return nil, fmt.Errorf("settlement: line %d: %q is not a valid address", i+1, line.To)
		case line.Amount.Sign() <= 0:
			return nil, fmt.Errorf("settlement: line %d: amount %s is not positive", i+1, line.Amount)
		case line.Asset == nil:
			return nil, fmt.Errorf("settlement: line %d has no asset", i+1)
		}
		ops = append(ops, &txnbuild.Payment{
			SourceAccount: from,
			Destination:   line.To,
			Amount:        line.Amount.String(),
			Asset:         line.Asset,
		})
	}
	return ops, nil
}

// SplitPayroll divides a batch into transaction-sized chunks.
//
// The chunks are NOT atomic with each other. Each one lands or does not, on its
// own, and a caller has to be able to resume from whichever chunk failed —
// which is why chunks are returned in a stable order and why the caller is
// expected to key each transaction's idempotency on the chunk index rather than
// on the batch.
func SplitPayroll(lines []PayrollLine, perTx int) [][]PayrollLine {
	if perTx <= 0 || perTx > PayrollChunkSize {
		perTx = PayrollChunkSize
	}
	if len(lines) == 0 {
		return nil
	}

	chunks := make([][]PayrollLine, 0, (len(lines)+perTx-1)/perTx)
	for start := 0; start < len(lines); start += perTx {
		end := start + perTx
		if end > len(lines) {
			end = len(lines)
		}
		chunks = append(chunks, lines[start:end])
	}
	return chunks
}

// TotalPayroll adds a batch up, per asset.
//
// Per asset, because adding across assets is meaningless and the money type
// deliberately cannot tell one from another — it is a scalar, and this is the
// layer that knows what the scalar counts.
func TotalPayroll(lines []PayrollLine) (map[string]money.Stroops, error) {
	totals := make(map[string]money.Stroops)
	for i, line := range lines {
		key, err := describeAsset(line.Asset)
		if err != nil {
			return nil, fmt.Errorf("settlement: line %d: %w", i+1, err)
		}
		sum, err := totals[key].Add(line.Amount)
		if err != nil {
			return nil, fmt.Errorf("settlement: total for %s: %w", key, err)
		}
		totals[key] = sum
	}
	return totals, nil
}
