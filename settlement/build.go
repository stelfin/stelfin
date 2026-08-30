package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Building a transaction out of several operations.
//
// The operation constructors in this package return []txnbuild.Operation rather
// than a transaction, so they compose: a payroll run and the trustline it needs
// go into one atomic envelope rather than two that can half-land.
//
// The pure/impure split mirrors BuildPayment. buildOn takes an account and
// touches no network, so a multi-operation builder stays testable offline;
// Build loads the account first, which is the only part that needs Horizon.

// MaxOperations is Stellar's per-transaction operation limit.
const MaxOperations = 100

// BuildRequest describes a transaction to assemble.
type BuildRequest struct {
	// Source pays the fee and, unless an operation names its own, sources every
	// operation.
	Source string
	// Operations are included in order. Order matters for more than
	// presentation: a SetOptions that lowers a threshold before the signers
	// that will meet it exist can brick an account inside one envelope.
	Operations []txnbuild.Operation
	Memo       txnbuild.Memo
	// Timeout bounds validity. Zero uses DefaultTimeout.
	//
	// Worth choosing deliberately for anything collecting signatures over time:
	// DefaultTimeout is three minutes, which is right for a payment someone
	// confirms on their phone and far too short for a treasury that needs three
	// people to approve.
	Timeout time.Duration
	// Preconditions overrides the time bounds entirely, for callers that need
	// more than a timeout.
	Preconditions *txnbuild.Preconditions
}

// Build assembles and returns an unsigned transaction.
func (c *Client) Build(ctx context.Context, req BuildRequest) (*txnbuild.Transaction, error) {
	if req.Source == "" {
		return nil, errors.New("settlement: build needs a source account")
	}
	account, err := c.LoadAccount(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	return c.buildOn(account, req)
}

// buildOn assembles a transaction against an already-loaded account.
func (c *Client) buildOn(account txnbuild.Account, req BuildRequest) (*txnbuild.Transaction, error) {
	switch {
	case len(req.Operations) == 0:
		return nil, errors.New("settlement: a transaction needs at least one operation")
	case len(req.Operations) > MaxOperations:
		return nil, fmt.Errorf(
			"settlement: %d operations, the protocol allows %d — split the batch",
			len(req.Operations), MaxOperations)
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	preconditions := txnbuild.Preconditions{
		TimeBounds: txnbuild.NewTimeout(int64(timeout.Seconds())),
	}
	if req.Preconditions != nil {
		preconditions = *req.Preconditions
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        account,
		IncrementSequenceNum: true,
		BaseFee:              c.baseFee,
		Memo:                 req.Memo,
		Preconditions:        preconditions,
		Operations:           req.Operations,
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: build transaction on %s: %w", req.Source, err)
	}
	return tx, nil
}
