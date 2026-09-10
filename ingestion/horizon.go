package ingestion

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/stelfin/stelfin/internal/money"
)

// OperationsAPI is the slice of Horizon this source needs.
//
// Operations, not Payments. The narrower endpoint returns only payment-shaped
// operations, and everything else a treasury does — creating an account,
// merging one away, swapping through the order book — is invisible to it.
// api/enroll.go documented the consequence for a year: a provisioning
// transaction that lands on chain while its finalisation step does not leaves a
// funded account nothing reconciles, because CreateAccount is not a payment.
type OperationsAPI interface {
	Operations(horizonclient.OperationRequest) (operations.OperationsPage, error)
}

// OperationsSource reads classic operations from Horizon.
type OperationsSource struct {
	api  OperationsAPI
	name string
}

// NewOperationsSource returns a source reading every classic operation.
func NewOperationsSource(api OperationsAPI, name string) (*OperationsSource, error) {
	if api == nil {
		return nil, fmt.Errorf("ingestion: operations source needs a Horizon client")
	}
	if name == "" {
		return nil, fmt.Errorf("ingestion: a source needs a name to track its cursor under")
	}
	return &OperationsSource{api: api, name: name}, nil
}

// Name is the cursor row this source tracks.
func (s *OperationsSource) Name() string { return s.name }

// Fetch returns one page of operations, translated.
func (s *OperationsSource) Fetch(_ context.Context, cursor string, limit uint) ([]Record, error) {
	page, err := s.api.Operations(horizonclient.OperationRequest{
		Cursor: cursor,
		Order:  horizonclient.OrderAsc,
		Limit:  limit,
		// Failed transactions moved no money. Including them would post entries
		// for value that never changed hands.
		IncludeFailed: false,
	})
	if err != nil {
		return nil, fmt.Errorf("ingestion: fetch operations after %q: %w", cursor, err)
	}

	out := make([]Record, 0, len(page.Embedded.Records))
	for _, op := range page.Embedded.Records {
		record, err := recordOf(op)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// recordOf translates one Horizon operation.
//
// An operation type this does not understand becomes a record with nothing in
// it. That is deliberate and is not the same as ignoring it: the cursor still
// advances past it, so an operation nobody has taught this about cannot wedge
// the stream — and the stream carries every tenant's money.
func recordOf(op operations.Operation) (Record, error) {
	base := Record{
		Cursor:     op.PagingToken(),
		ID:         "horizon:op:" + op.GetID(),
		TxHash:     op.GetTransactionHash(),
		OccurredAt: op.GetBase().LedgerCloseTime,
	}
	if !op.IsTransactionSuccessful() {
		return base, nil
	}

	switch o := op.(type) {
	case operations.Payment:
		amount, err := money.Parse(o.Amount)
		if err != nil {
			return Record{}, fmt.Errorf("ingestion: operation %s has unparseable amount %q: %w",
				o.ID, o.Amount, err)
		}
		base.Movements = []Movement{{
			From: o.From, To: o.To,
			AssetType: o.Asset.Type, AssetCode: o.Asset.Code, AssetIssuer: o.Asset.Issuer,
			Amount: amount,
		}}

	case operations.CreateAccount:
		// The operation that closes the enrolment reconciliation hole. A
		// sponsored account is brought into existence here and nowhere else,
		// and its starting balance is real XLM leaving the funder.
		amount, err := money.Parse(o.StartingBalance)
		if err != nil {
			return Record{}, fmt.Errorf(
				"ingestion: operation %s has unparseable starting balance %q: %w",
				o.ID, o.StartingBalance, err)
		}
		if amount.Sign() > 0 {
			base.Movements = []Movement{{
				From: o.Funder, To: o.Account, AssetType: "native", Amount: amount,
			}}
		}

	case operations.PathPayment:
		// Strict-receive. The destination gets exactly Amount of the
		// destination asset; the source gives up SourceAmount of a different
		// one. Two movements, not one, because they are two assets — recording
		// only the destination leg would credit money that arrived from
		// nowhere.
		movements, err := pathMovements(o.Payment, o.SourceAmount,
			o.SourceAssetType, o.SourceAssetCode, o.SourceAssetIssuer)
		if err != nil {
			return Record{}, err
		}
		base.Movements = movements

	case operations.PathPaymentStrictSend:
		movements, err := pathMovements(o.Payment, o.SourceAmount,
			o.SourceAssetType, o.SourceAssetCode, o.SourceAssetIssuer)
		if err != nil {
			return Record{}, err
		}
		base.Movements = movements

	case operations.AccountMerge:
		// Horizon states who merged into whom and not how much: the balance
		// swept is only in the effects. So this records the fact that can be
		// known — the account stopped existing — rather than inventing the one
		// that cannot.
		//
		// The balance itself is reconciled the next time the destination is
		// read, which is the honest ordering: a number nobody can source is
		// worse than a number that arrives late.
		base.Closed = o.Account
	}

	return base, nil
}

// pathMovements turns a path payment into its two legs.
func pathMovements(
	p operations.Payment, sourceAmount, sourceType, sourceCode, sourceIssuer string,
) ([]Movement, error) {
	received, err := money.Parse(p.Amount)
	if err != nil {
		return nil, fmt.Errorf("ingestion: operation %s has unparseable amount %q: %w",
			p.ID, p.Amount, err)
	}
	sent, err := money.Parse(sourceAmount)
	if err != nil {
		return nil, fmt.Errorf("ingestion: operation %s has unparseable source amount %q: %w",
			p.ID, sourceAmount, err)
	}

	// Two half-movements rather than one transfer, because the assets differ.
	// The sender's leg has no tracked destination and the receiver's has no
	// tracked source, so each posts against its own org's counterparty — which
	// is exactly what happened: one asset left, another arrived.
	return []Movement{
		{
			From: p.From, To: "",
			AssetType: sourceType, AssetCode: sourceCode, AssetIssuer: sourceIssuer,
			Amount: sent,
		},
		{
			From: "", To: p.To,
			AssetType: p.Asset.Type, AssetCode: p.Asset.Code, AssetIssuer: p.Asset.Issuer,
			Amount: received,
		},
	}, nil
}
