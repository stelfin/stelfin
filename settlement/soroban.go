package settlement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// Soroban submission is a different protocol from a classic payment, and
// pretending otherwise is how a contract call quietly costs a treasury money.
//
// A classic transaction is built, signed and submitted. A Soroban one has a
// step in the middle that changes the envelope: simulation returns the
// footprint, the resource fee and — depending on how it was built — the
// authorisation entries, and all of that has to be written into the transaction
// before anybody signs it. Sign first and the signature covers a transaction
// with no footprint, which the network rejects.
//
// The rules in assemble.go are what the SDK does not enforce, and each of them
// is a way to lose money or authority that nothing downstream would catch.

// MaxResourceFee caps what a single contract call may bid for resources.
//
// Nothing in the protocol caps this, and simulation reports whatever the
// contract's execution happened to cost. A contract designed to be hostile —
// or merely one with a loop whose bound is attacker-controlled — can make
// simulation report a resource fee of hundreds of XLM, and a client that
// trusted it would sign that bid.
//
// 5 XLM is far above any honest call and far below an amount worth stealing.
// It is a ceiling rather than a target: the fee actually paid is what execution
// uses, so a generous ceiling costs nothing on the normal path.
const MaxResourceFee = money.Stroops(50_000_000)

// SorobanTimeout is how long a contract call's envelope stays valid.
const SorobanTimeout = 5 * time.Minute

var (
	// ErrNoSoroban reports a deployment with no RPC endpoint configured.
	ErrNoSoroban = errors.New("settlement: no Soroban RPC endpoint is configured")

	// ErrSimulationFailed reports a contract call that would not succeed.
	//
	// Worth its own error because it is the good case: the call was tried
	// against current ledger state and refused, at no cost and with no
	// signature. The alternative is finding out on chain, having paid.
	ErrSimulationFailed = errors.New("settlement: the contract call would fail")

	// ErrResourceFeeTooHigh reports a simulation bidding more than the ceiling.
	ErrResourceFeeTooHigh = errors.New("settlement: this call bids more for resources than allowed")

	// ErrArchivedState reports ledger entries that must be restored first.
	//
	// Deliberately not handled automatically. Restoring costs money, changes
	// what the transaction does, and produces a different envelope from the one
	// a person was shown — so it is surfaced and left to a human.
	ErrArchivedState = errors.New("settlement: this call needs archived ledger state restored first")

	// ErrNotSoroban reports a transaction that is not a single contract call.
	ErrNotSoroban = errors.New("settlement: not a single Soroban operation")

	// ErrTryAgainLater reports an RPC that declined to queue the transaction.
	//
	// Backpressure, and it says nothing about the transaction: it was not
	// queued, so there is nothing on chain and nothing to reconcile.
	ErrTryAgainLater = errors.New("settlement: the network asked us to try again later")

	// ErrRejected reports a transaction refused before inclusion.
	//
	// The only submission outcome where nothing happened. Everything else —
	// including a FAILED transaction — consumed a fee and a sequence number.
	ErrRejected = errors.New("settlement: the network rejected this transaction")
)

// SorobanAPI is the slice of rpcclient.Client this package uses.
//
// Narrowed for the same reason HorizonAPI is: the four submission statuses and
// the two kinds of NOT_FOUND are testable against a fake, and standing up an
// RPC node to prove that a DUPLICATE is not a failure would mean never proving
// it.
type SorobanAPI interface {
	SimulateTransaction(
		ctx context.Context, req protocol.SimulateTransactionRequest,
	) (protocol.SimulateTransactionResponse, error)
	SendTransaction(
		ctx context.Context, req protocol.SendTransactionRequest,
	) (protocol.SendTransactionResponse, error)
	GetTransaction(
		ctx context.Context, req protocol.GetTransactionRequest,
	) (protocol.GetTransactionResponse, error)
}

// WithSoroban returns a copy of c that can also talk to a Soroban RPC endpoint.
func (c *Client) WithSoroban(rpc SorobanAPI) *Client {
	out := *c
	out.soroban = rpc
	return &out
}

// DialSoroban returns a copy of c wired to the RPC endpoint at url.
func (c *Client) DialSoroban(url string, httpClient *http.Client) (*Client, error) {
	if url == "" {
		return nil, ErrNoSoroban
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return c.WithSoroban(rpcclient.NewClient(url, httpClient)), nil
}

// HasSoroban reports whether contract calls are available at all.
func (c *Client) HasSoroban() bool { return c.soroban != nil }

// Simulation is what simulation said, in the terms a caller acts on.
type Simulation struct {
	// TransactionData is the footprint and resource limits, as XDR, which the
	// assembled envelope must carry.
	TransactionData string
	// MinResourceFee is what the network says this call costs in resources, on
	// top of the ordinary per-operation fee.
	MinResourceFee money.Stroops
	// Auth is the authorisation entries simulation recorded, if any.
	//
	// Only meaningful when the call was simulated with no auth supplied. A call
	// that arrived already carrying auth must keep its own — see assemble.
	Auth []string
	// ReturnValue is what the function returned, as XDR. Empty for a call that
	// returns void.
	ReturnValue string
}

// Simulate runs a contract call against current ledger state without submitting
// it.
//
// This is the step that makes contract calls safe to offer at all: the call is
// tried, the failure modes surface before anybody signs, and the cost is known
// rather than guessed.
func (c *Client) Simulate(ctx context.Context, tx *txnbuild.Transaction) (*Simulation, error) {
	if c.soroban == nil {
		return nil, ErrNoSoroban
	}
	if _, err := sorobanOp(tx); err != nil {
		return nil, err
	}

	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("settlement: encode for simulation: %w", err)
	}

	resp, err := c.soroban.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{
		Transaction: envelope,
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: simulate: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrSimulationFailed, resp.Error)
	}

	// Archived state. Restoring it is a separate transaction with its own cost
	// and its own description, so this refuses rather than quietly doing it —
	// a caller who wanted a restore can ask for one.
	if resp.RestorePreamble != nil {
		return nil, fmt.Errorf("%w (restoring would cost %s)",
			ErrArchivedState, money.Stroops(resp.RestorePreamble.MinResourceFee))
	}

	fee := money.Stroops(resp.MinResourceFee)
	if fee > MaxResourceFee {
		return nil, fmt.Errorf("%w: %s, the ceiling is %s",
			ErrResourceFeeTooHigh, fee, MaxResourceFee)
	}
	if resp.TransactionDataXDR == "" {
		return nil, errors.New("settlement: simulation returned no transaction data")
	}

	out := &Simulation{TransactionData: resp.TransactionDataXDR, MinResourceFee: fee}
	if len(resp.Results) > 0 {
		if auth := resp.Results[0].AuthXDR; auth != nil {
			out.Auth = *auth
		}
		if ret := resp.Results[0].ReturnValueXDR; ret != nil {
			out.ReturnValue = *ret
		}
	}
	return out, nil
}

// SubmitSoroban sends an assembled, signed contract call and waits for it to
// land.
//
// The four statuses are four different facts and are treated as such. PENDING
// is queued. DUPLICATE means the network already has this exact transaction,
// which on a retry is the expected answer and not a failure. TRY_AGAIN_LATER is
// backpressure and says nothing about the transaction. ERROR is a definite
// rejection before inclusion, and the only one where nothing happened.
func (c *Client) SubmitSoroban(
	ctx context.Context, tx *txnbuild.Transaction,
) (Result, error) {
	if c.soroban == nil {
		return Result{}, ErrNoSoroban
	}

	hash, err := tx.HashHex(c.network)
	if err != nil {
		return Result{}, fmt.Errorf("settlement: hash transaction: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return Result{}, fmt.Errorf("settlement: encode transaction: %w", err)
	}

	// The envelope's own expiry, kept because it is what makes one kind of
	// NOT_FOUND definite: past this, the transaction can never be included.
	maxTime := tx.Timebounds().MaxTime

	resp, err := c.soroban.SendTransaction(ctx,
		protocol.SendTransactionRequest{Transaction: envelope})
	if err != nil {
		// The request failed, which says nothing about whether the transaction
		// reached the network. Look it up rather than concluding.
		//
		// No send ledger to anchor retention against, because the send is what
		// failed — so a later NOT_FOUND here is judged on time bounds alone.
		return c.awaitSoroban(ctx, hash, maxTime, 0)
	}

	switch resp.Status {
	case stellarcore.TXStatusPending, stellarcore.TXStatusDuplicate:
		// DUPLICATE is not an error. It is what a correct retry looks like
		// after an ambiguous first attempt, and treating it as a failure is how
		// a payment gets sent twice.
		return c.awaitSoroban(ctx, hash, maxTime, resp.LatestLedger)

	case stellarcore.TXStatusTryAgainLater:
		return Result{}, fmt.Errorf("%w (hash %s)", ErrTryAgainLater, hash)

	case stellarcore.TXStatusError:
		return Result{}, fmt.Errorf("%w: %s",
			ErrRejected, describeCoreError(resp.ErrorResultXDR))

	default:
		return Result{}, fmt.Errorf("settlement: unrecognised submission status %q", resp.Status)
	}
}

// ErrRetentionGap reports an RPC that can no longer answer for the window a
// transaction was submitted in.
//
// The RPC keeps a bounded history. Once it has forgotten the ledgers around a
// submission, its NOT_FOUND stops meaning "this did not happen" and starts
// meaning "I cannot say" — and the two look identical. Reported rather than
// resolved, because concluding "did not happen" here is how a transaction that
// landed gets submitted a second time.
var ErrRetentionGap = errors.New("settlement: the RPC no longer covers this transaction's window")

// awaitSoroban polls until the transaction is in a ledger, or until it can no
// longer be.
//
// NOT_FOUND is the whole difficulty. It has three meanings and they are not
// interchangeable: not included yet, never going to be included, and outside
// the history this RPC keeps. Each is decided here from something other than
// the status itself, because the status cannot tell them apart.
func (c *Client) awaitSoroban(
	ctx context.Context, hash string, maxTime int64, sentAtLedger uint32,
) (Result, error) {
	const interval = time.Second

	for {
		resp, err := c.soroban.GetTransaction(ctx, protocol.GetTransactionRequest{Hash: hash})
		if err != nil {
			return Result{}, fmt.Errorf("settlement: look up %s: %w", hash, err)
		}

		switch resp.Status {
		case protocol.TransactionStatusSuccess:
			return Result{
				Hash:     hash,
				Ledger:   int32(resp.Ledger),
				ClosedAt: time.Unix(resp.LedgerCloseTime, 0).UTC(),
			}, nil

		case protocol.TransactionStatusFailed:
			// A FAILED Soroban transaction is on chain. It consumed the fee and
			// the sequence number and changed nothing else, so it is neither a
			// success nor a non-event — a caller that retried it would build
			// against a sequence that has already moved.
			return Result{
				Hash:     hash,
				Ledger:   int32(resp.Ledger),
				ClosedAt: time.Unix(resp.LedgerCloseTime, 0).UTC(),
				Failed:   true,
			}, nil

		case protocol.TransactionStatusNotFound:
			// The history no longer covers when this was sent, so this RPC's
			// "not found" is about its own retention. Horizon is the authority
			// from here, and saying so is the only honest answer.
			if sentAtLedger > 0 && resp.OldestLedger > sentAtLedger {
				return Result{}, fmt.Errorf(
					"%w: sent at ledger %d, and its history now starts at %d (hash %s)",
					ErrRetentionGap, sentAtLedger, resp.OldestLedger, hash)
			}
			// Past its own time bounds and still not included, so it never can
			// be. Definite, and the only NOT_FOUND that is.
			if maxTime > 0 && resp.LatestLedgerCloseTime > maxTime {
				return Result{}, fmt.Errorf("%w: %s expired without being included",
					ErrNotFound, hash)
			}

		default:
			return Result{}, fmt.Errorf("settlement: unrecognised transaction status %q", resp.Status)
		}

		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("settlement: gave up waiting for %s: %w", hash, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// describeCoreError turns a TransactionResult XDR into something a log can
// carry. Best effort: a garbled result should not replace the failure with a
// decoding failure.
func describeCoreError(resultXDR string) string {
	if resultXDR == "" {
		return "no result returned"
	}
	var result xdr.TransactionResult
	if err := xdr.SafeUnmarshalBase64(resultXDR, &result); err != nil {
		return "undecodable result"
	}
	return result.Result.Code.String()
}
