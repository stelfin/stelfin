// Package settlement talks to the Stellar network.
//
// It owns transaction construction, signing coordination and submission. It
// deliberately owns no policy: whether a user is allowed to move money is
// decided upstream, and this package only turns an authorised decision into a
// transaction and reports what the network did with it.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// DefaultBaseFee is the per-operation fee bid, in stroops. Stellar's network
// minimum is 100; bidding above it buys priority when the ledger is congested.
// Users never pay this — the treasury does, via sponsorship or a fee-bump.
const DefaultBaseFee int64 = 10_000

// DefaultTimeout bounds how long a built transaction stays valid. Timebounds
// are not optional: without them a transaction has no expiry, so a submission
// that appears to fail can still land arbitrarily later, and its hash is not a
// reliable thing to reconcile against.
const DefaultTimeout = 180 * time.Second

// Config describes which network to talk to.
type Config struct {
	// HorizonURL is the Horizon instance to use. Production should point at a
	// self-hosted instance: the public SDF one is rate-limited and carries no
	// availability guarantee.
	HorizonURL string

	// NetworkPassphrase selects the network and is mixed into every signature,
	// so a testnet-signed transaction can never be replayed on mainnet.
	NetworkPassphrase string

	// BaseFee is the per-operation fee bid in stroops. Zero means DefaultBaseFee.
	BaseFee int64
}

// Client is a handle on a Stellar network.
type Client struct {
	horizon horizonAPI
	network string
	baseFee int64
}

// horizonAPI is the slice of horizonclient.Client this package uses. Narrowing
// it keeps the surface testable without standing up a Horizon instance.
type horizonAPI interface {
	AccountDetail(horizonclient.AccountRequest) (horizon.Account, error)
	SubmitTransactionWithOptions(*txnbuild.Transaction, horizonclient.SubmitTxOpts) (horizon.Transaction, error)
	SubmitFeeBumpTransactionWithOptions(*txnbuild.FeeBumpTransaction, horizonclient.SubmitTxOpts) (horizon.Transaction, error)
	TransactionDetail(string) (horizon.Transaction, error)
}

// New returns a Client for the network described by cfg.
func New(cfg Config) (*Client, error) {
	if cfg.HorizonURL == "" {
		return nil, errors.New("settlement: horizon url is required")
	}
	if cfg.NetworkPassphrase == "" {
		return nil, errors.New("settlement: network passphrase is required")
	}
	fee := cfg.BaseFee
	if fee == 0 {
		fee = DefaultBaseFee
	}
	return &Client{
		horizon: &horizonclient.Client{HorizonURL: cfg.HorizonURL},
		network: cfg.NetworkPassphrase,
		baseFee: fee,
	}, nil
}

// Network returns the passphrase this client signs with.
func (c *Client) Network() string { return c.network }

// Result describes the outcome of a submission.
type Result struct {
	// Hash is the transaction hash. It is deterministic from the signed
	// envelope, which is what makes recovery from an ambiguous submission
	// possible.
	Hash string
	// Ledger is the ledger sequence the transaction landed in.
	Ledger int32
	// ClosedAt is the ledger close time: the authoritative "when" for anything
	// this transaction caused.
	ClosedAt time.Time
	// AlreadyKnown reports that the transaction was found on chain rather than
	// accepted by this submission — a retry of something that already landed.
	AlreadyKnown bool
}

// ErrNotFound reports that a transaction is not on the ledger.
var ErrNotFound = errors.New("settlement: transaction not found")

// LoadAccount fetches an account's current state, primarily for its sequence
// number.
func (c *Client) LoadAccount(ctx context.Context, address string) (*horizon.Account, error) {
	acct, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		return nil, fmt.Errorf("settlement: load account %s: %w", address, err)
	}
	return &acct, nil
}

// Submit sends a signed transaction and reports what the network did.
//
// A failed submission is never treated as proof that nothing happened. When the
// request itself fails — timeout, connection reset, a 5xx from Horizon — the
// transaction may still have been accepted, so this looks the hash up before
// concluding anything. Blindly retrying instead would risk paying twice.
func (c *Client) Submit(ctx context.Context, tx *txnbuild.Transaction) (Result, error) {
	hash, err := tx.HashHex(c.network)
	if err != nil {
		return Result{}, fmt.Errorf("settlement: hash transaction: %w", err)
	}

	resp, submitErr := c.horizon.SubmitTransactionWithOptions(
		tx, horizonclient.SubmitTxOpts{SkipMemoRequiredCheck: true})
	if submitErr == nil {
		return resultFrom(resp, false), nil
	}
	return c.resolveAmbiguous(ctx, hash, submitErr)
}

// SubmitFeeBump sends a fee-bumped transaction, with the same ambiguity
// handling as Submit.
func (c *Client) SubmitFeeBump(ctx context.Context, tx *txnbuild.FeeBumpTransaction) (Result, error) {
	hash, err := tx.HashHex(c.network)
	if err != nil {
		return Result{}, fmt.Errorf("settlement: hash fee-bump transaction: %w", err)
	}

	resp, submitErr := c.horizon.SubmitFeeBumpTransactionWithOptions(
		tx, horizonclient.SubmitTxOpts{SkipMemoRequiredCheck: true})
	if submitErr == nil {
		return resultFrom(resp, false), nil
	}
	return c.resolveAmbiguous(ctx, hash, submitErr)
}

// resolveAmbiguous decides what a failed submission actually meant.
//
// Horizon rejecting a transaction outright (a 400 with a result code) is a
// definite "did not happen". Anything else is ambiguous, and the ledger is the
// only authority that can settle it.
func (c *Client) resolveAmbiguous(ctx context.Context, hash string, submitErr error) (Result, error) {
	if isDefiniteRejection(submitErr) {
		return Result{}, fmt.Errorf("settlement: transaction %s rejected: %w", hash, submitErr)
	}

	found, lookupErr := c.LookupTransaction(ctx, hash)
	switch {
	case lookupErr == nil:
		// It landed despite the failed request. Reporting this as an error
		// would invite a retry that pays a second time.
		found.AlreadyKnown = true
		return found, nil
	case errors.Is(lookupErr, ErrNotFound):
		return Result{}, fmt.Errorf(
			"settlement: submission of %s failed and it is not on the ledger: %w", hash, submitErr)
	default:
		// Neither outcome is established. The caller must not retry blind; the
		// hash is the handle for resolving this later.
		return Result{}, fmt.Errorf(
			"settlement: outcome of %s is unknown (submit: %v; lookup: %w)", hash, submitErr, lookupErr)
	}
}

// LookupTransaction reports whether a transaction is on the ledger.
func (c *Client) LookupTransaction(ctx context.Context, hash string) (Result, error) {
	tx, err := c.horizon.TransactionDetail(hash)
	if err != nil {
		if horizonclient.IsNotFoundError(err) {
			return Result{}, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return Result{}, fmt.Errorf("settlement: look up transaction %s: %w", hash, err)
	}
	return resultFrom(tx, false), nil
}

func resultFrom(tx horizon.Transaction, alreadyKnown bool) Result {
	return Result{
		Hash:         tx.Hash,
		Ledger:       tx.Ledger,
		ClosedAt:     tx.LedgerCloseTime,
		AlreadyKnown: alreadyKnown,
	}
}

// isDefiniteRejection reports whether Horizon told us the transaction was
// invalid, as opposed to the request failing for some other reason. Only a
// structured Horizon problem carrying transaction result codes is conclusive.
func isDefiniteRejection(err error) bool {
	problem := horizonclient.GetError(err)
	if problem == nil {
		return false
	}
	codes, codeErr := problem.ResultCodes()
	return codeErr == nil && codes != nil
}
