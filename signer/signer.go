// Package signer is the seam between stelfin and anything that holds a key.
//
// One interface, four implementations, and the interesting one signs nothing.
//
// A DAO's treasury is signed for by its own members, on their own devices, with
// keys this process never sees. So the "signer" for a treasury is the one that
// contributes no signature and reports how much weight is still needed — and
// the router has no branch that returns a key-holding signer for a treasury at
// all. That is the rule the whole product rests on, and it is carried by the
// type system rather than by a comment.
//
// The one key this process does hold is the operator's: it sponsors reserves
// and pays fee bumps, and it can move nothing else. That is a deliberate
// blast-radius decision. A leaked operator key costs the fee float, not a
// treasury — worth more than any amount of key-storage hardening, because it
// changes the consequence rather than the probability.
package signer

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/ledger"
)

// Purpose says what a signature is for, and is what the router dispatches on.
type Purpose string

const (
	// PurposeSponsor is the operator paying the reserves for a new account.
	PurposeSponsor Purpose = "sponsor"
	// PurposeFeeBump is the operator covering someone else's fee.
	PurposeFeeBump Purpose = "fee_bump"
	// PurposeTreasury is a DAO's own account signing. Never this process.
	PurposeTreasury Purpose = "treasury"
	// PurposeChallenge is SEP-10 web authentication.
	//
	// A separate purpose because it must reach a separate key: it signs
	// arbitrary attacker-chosen material, and the account behind it holds
	// nothing.
	PurposeChallenge Purpose = "challenge"
)

var (
	// ErrNetworkMismatch reports a request built for a different network.
	ErrNetworkMismatch = errors.New("signer: request is for a different network")

	// ErrNoSigner reports a purpose this router cannot serve.
	ErrNoSigner = errors.New("signer: nothing configured to sign for this purpose")

	// ErrMainnetKeyInProcess reports an attempt to hold a mainnet key in the
	// process without an explicit acknowledgement.
	ErrMainnetKeyInProcess = errors.New("signer: refusing to hold a mainnet key in the process")
)

// Request is what needs signing. Exactly one of Tx and FeeBump is set.
type Request struct {
	Org     ledger.OrgID
	Purpose Purpose
	// Network is the passphrase the envelope was built against. Checked rather
	// than assumed: a mismatch means the caller built for the wrong network,
	// and a signature over it would be rejected later for a reason nobody would
	// connect to this.
	Network string
	// Account is the account being asked to sign.
	Account string

	Tx      *txnbuild.Transaction
	FeeBump *txnbuild.FeeBumpTransaction
}

func (r Request) validate() error {
	switch {
	case r.Purpose == "":
		return errors.New("signer: request has no purpose")
	case r.Network == "":
		return errors.New("signer: request has no network")
	case r.Tx == nil && r.FeeBump == nil:
		return errors.New("signer: request has nothing to sign")
	case r.Tx != nil && r.FeeBump != nil:
		return errors.New("signer: request carries both a transaction and a fee bump")
	}
	return nil
}

// Response reports what a signer contributed.
type Response struct {
	Tx      *txnbuild.Transaction
	FeeBump *txnbuild.FeeBumpTransaction

	// Complete reports that no further signature is needed.
	//
	// False is not an error. For an M-of-N treasury it is the ordinary answer
	// until the last approver signs, and the caller's job is to persist what it
	// has and wait — not to retry.
	Complete bool

	// Have and Need are collected weight against the threshold required.
	//
	// Advisory, and deliberately so. The network decides whether a transaction
	// is authorised, against the account as it stands at inclusion. These two
	// numbers exist so the bot can say "two more signatures" instead of
	// guessing, and being wrong about them costs a misleading message rather
	// than a wrong outcome.
	Have, Need int32
}

// Signer contributes signatures. It never holds a member's key.
type Signer interface {
	Sign(ctx context.Context, req Request) (Response, error)
	// Address is the account this signer signs as, so a caller can name it in
	// an envelope before asking for a signature.
	Address(ctx context.Context) (string, error)
}

// Router sends each request to whatever should answer it.
type Router struct {
	network  string
	operator Signer
	webAuth  Signer
	treasury TreasuryLookup
	auth     AuthChecker
}

// TreasuryLookup reports how a treasury signs.
//
// An interface rather than a store dependency, so this package stays about
// signing: what it needs to know is "single, multisig, or smart account", and
// where the numbers come from is somebody else's concern.
type TreasuryLookup interface {
	// SignerSet returns the account's signers and thresholds as the network
	// currently reports them.
	//
	// Currently, not as cached. A stale signer set is exactly what someone being
	// removed would exploit, and it is the input to every weight calculation
	// below.
	SignerSet(ctx context.Context, org ledger.OrgID, account string) (Set, error)
}

// Set is an account's signers and thresholds.
type Set struct {
	// Signers maps address to weight, including the master key.
	Signers map[string]int32
	// Medium is the threshold a payment needs. The one that matters for almost
	// everything a treasury does.
	Medium int32
	// Contract is set when the account is a Soroban smart account, whose
	// authorisation is decided by contract code rather than by weight.
	Contract string
}

// Config wires a router.
type Config struct {
	// Network is the passphrase every request must match.
	Network string
	// Operator signs sponsorships and fee bumps.
	Operator Signer
	// WebAuth signs SEP-10 challenges. Optional.
	WebAuth Signer
	// Treasuries reports how each treasury signs.
	Treasuries TreasuryLookup
	// Auth decides whether a Soroban smart account authorises an envelope.
	// Optional: without it, a contract-controlled treasury reports
	// ErrAuthUndecidable rather than a guess.
	Auth AuthChecker
}

// NewRouter returns a Router.
func NewRouter(cfg Config) (*Router, error) {
	switch {
	case cfg.Network == "":
		return nil, errors.New("signer: router needs a network")
	case cfg.Operator == nil:
		return nil, errors.New("signer: router needs an operator signer")
	case cfg.Treasuries == nil:
		return nil, errors.New("signer: router needs a treasury lookup")
	}
	return &Router{
		network:  cfg.Network,
		operator: cfg.Operator,
		webAuth:  cfg.WebAuth,
		treasury: cfg.Treasuries,
		auth:     cfg.Auth,
	}, nil
}

// Sign routes a request.
//
// Note what is missing from the switch: there is no case that returns a
// key-holding signer for a treasury. A DAO's account is signed for by its
// members, and the absence of that branch is what makes it impossible for a
// future change to quietly add one without someone noticing this comment.
func (r *Router) Sign(ctx context.Context, req Request) (Response, error) {
	if err := req.validate(); err != nil {
		return Response{}, err
	}
	if req.Network != r.network {
		return Response{}, fmt.Errorf("%w: request for %q, this router signs %q",
			ErrNetworkMismatch, req.Network, r.network)
	}

	switch req.Purpose {
	case PurposeSponsor, PurposeFeeBump:
		return r.operator.Sign(ctx, req)

	case PurposeChallenge:
		if r.webAuth == nil {
			return Response{}, fmt.Errorf("%w: %s", ErrNoSigner, req.Purpose)
		}
		return r.webAuth.Sign(ctx, req)

	case PurposeTreasury:
		set, err := r.treasury.SignerSet(ctx, req.Org, req.Account)
		if err != nil {
			return Response{}, err
		}
		if set.Contract != "" {
			return NewSmartAccount(set.Contract, r.auth).Sign(ctx, req)
		}
		return NewExternal(set).Sign(ctx, req)

	default:
		return Response{}, fmt.Errorf("%w: %q", ErrNoSigner, req.Purpose)
	}
}

// OperatorAddress reports the account the operator signs as.
func (r *Router) OperatorAddress(ctx context.Context) (string, error) {
	return r.operator.Address(ctx)
}
