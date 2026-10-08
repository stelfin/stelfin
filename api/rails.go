package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// The limits stelfin enforces itself, above whatever the chain enforces.
//
// None of this is authority over money: a treasury's signers can always build
// and sign a transaction somewhere else, and the network will take it. These
// are limits on what this deployment will put in front of people and submit on
// their behalf — which is the part an operator answers for.

var (
	// ErrReadOnly reports a deployment that has been switched to read-only.
	//
	// The deployment-wide kill switch. Everything that reads still works;
	// nothing new is built, and nothing is submitted.
	ErrReadOnly = errors.New("api: this deployment is read-only")

	// ErrOverCeiling reports a payment larger than a spend ceiling allows.
	ErrOverCeiling = errors.New("api: over a spend ceiling")

	// ErrDestinationNotReady reports a destination that could not receive the
	// payment as things stand on chain.
	ErrDestinationNotReady = errors.New("api: the destination cannot receive this payment")
)

// CeilingError says which ceiling a payment ran into, in terms a reply can use.
type CeilingError struct {
	// Scope is "workspace" or "deployment": who set the limit, and so who can
	// change it.
	Scope string
	// Window is "payment" for the per-payment ceiling and "day" for the daily.
	Window string
	// Limit is the ceiling; Asked is what this payment would have brought the
	// figure to.
	Limit, Asked money.Stroops
}

func (e *CeilingError) Error() string {
	return fmt.Sprintf("api: %s %s ceiling is %s, this would make %s", e.Scope, e.Window, e.Limit, e.Asked)
}

func (e *CeilingError) Unwrap() error { return ErrOverCeiling }

// DestinationError says why a destination cannot be paid.
type DestinationError struct {
	Address string
	// Reason is a sentence fragment that follows the address: "does not exist
	// on this network", "has no USDC trustline".
	Reason string
}

func (e *DestinationError) Error() string {
	return fmt.Sprintf("api: %s %s", e.Address, e.Reason)
}

func (e *DestinationError) Unwrap() error { return ErrDestinationNotReady }

// writable refuses when the deployment is read-only.
//
// Two switches, either of which is enough. The variable is read once, at boot.
// The file is looked for on every write, so an operator can stop everything in
// the time it takes to create it — and, if it cannot be looked for at all, the
// answer is read-only, because a kill switch that fails open is not one.
func (s *Service) writable() error {
	if s.cfg.ReadOnly {
		return ErrReadOnly
	}
	if path := s.cfg.ReadOnlyFile; path != "" {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			return ErrReadOnly
		case errors.Is(err, fs.ErrNotExist):
		default:
			return fmt.Errorf("%w: cannot tell whether %s exists: %v", ErrReadOnly, path, err)
		}
	}
	return nil
}

// checkSpend holds a treasury payment against every ceiling that applies.
//
// Four of them: per payment and per day, for the workspace and for the whole
// deployment. The workspace's are its own choice; the deployment's are the
// operator's, and exist because the operator is the one whose name is on the
// service when a workspace with no ceiling of its own proposes something
// absurd.
//
// The daily sums are read and compared without a lock, so two proposals racing
// each other can together exceed a daily ceiling by one payment. That is
// accepted: every proposal still needs its signatures, and the alternative is
// serialising every proposal in the deployment behind one row.
func (s *Service) checkSpend(ctx context.Context, org ledger.OrgID, amount money.Stroops) error {
	policy, err := s.store.SpendPolicyOf(ctx, org)
	if err != nil {
		return err
	}

	if limit := policy.PerPayment; limit > 0 && amount > limit {
		return &CeilingError{Scope: "workspace", Window: "payment", Limit: limit, Asked: amount}
	}
	if limit := s.cfg.MaxPayment; limit > 0 && amount > limit {
		return &CeilingError{Scope: "deployment", Window: "payment", Limit: limit, Asked: amount}
	}

	since := time.Now().Add(-24 * time.Hour)
	if limit := policy.Daily; limit > 0 {
		so, err := s.store.ProposedSince(ctx, org, since)
		if err != nil {
			return err
		}
		if so+amount > limit {
			return &CeilingError{Scope: "workspace", Window: "day", Limit: limit, Asked: so + amount}
		}
	}
	if limit := s.cfg.MaxDaily; limit > 0 {
		so, err := s.store.ProposedSince(ctx, 0, since)
		if err != nil {
			return err
		}
		if so+amount > limit {
			return &CeilingError{Scope: "deployment", Window: "day", Limit: limit, Asked: so + amount}
		}
	}
	return nil
}

// preflight checks that a destination can receive the configured asset, before
// anyone is asked to sign anything.
//
// On Stellar a payment to an account that does not exist, or that holds no
// trustline to the asset, fails — and it fails at submission, after every
// signature has been collected. Signatures are the one resource a DAO has, and
// finding this out with three of them spent means building the proposal again
// and asking everyone again.
//
// It is a courtesy and not a guarantee. The destination can close its trustline
// between this check and execution, and the network remains the authority.
func (s *Service) preflight(ctx context.Context, destination string, amount money.Stroops) error {
	credit, ok := s.cfg.Asset.(txnbuild.CreditAsset)

	account, err := s.settle.LoadAccount(ctx, destination)
	if err != nil {
		// By status, through the wrapping. The client's own IsNotFoundError
		// does not look through an error that has been wrapped on the way up,
		// and settlement wraps this one.
		var problem *horizonclient.Error
		if errors.As(err, &problem) && problem.Problem.Status == http.StatusNotFound {
			return &DestinationError{Address: destination, Reason: "does not exist on this network yet"}
		}
		return err
	}
	if !ok {
		// The native asset needs no trustline; an existing account can hold it.
		return nil
	}
	if destination == credit.Issuer {
		// Paying the issuer burns the asset. Unusual, but it needs no trustline.
		return nil
	}

	for _, b := range account.Balances {
		if b.Asset.Code != credit.Code || b.Asset.Issuer != credit.Issuer {
			continue
		}
		if b.IsAuthorized != nil && !*b.IsAuthorized {
			return &DestinationError{
				Address: destination,
				Reason:  fmt.Sprintf("holds a %s trustline the issuer has not authorised", credit.Code),
			}
		}
		held, err := money.Parse(b.Balance)
		if err != nil {
			return fmt.Errorf("api: read destination balance: %w", err)
		}
		limit, err := money.Parse(b.Limit)
		if err != nil {
			return fmt.Errorf("api: read destination trustline limit: %w", err)
		}
		if held+amount > limit {
			return &DestinationError{
				Address: destination,
				Reason:  fmt.Sprintf("has a %s trustline limit this payment would exceed", credit.Code),
			}
		}
		return nil
	}
	return &DestinationError{
		Address: destination,
		Reason:  fmt.Sprintf("has no %s trustline, so it cannot receive %s", credit.Code, credit.Code),
	}
}
