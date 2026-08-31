package signer

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
)

// ErrAuthUndecidable reports that whether an envelope is authorised cannot be
// answered here.
//
// Returned rather than a quiet Complete=false, because those two answers lead
// somewhere different. "Not complete" tells the caller to wait for a signature;
// for a smart account no signature is ever going to arrive, so waiting is
// waiting forever. An error at least surfaces.
var ErrAuthUndecidable = errors.New("signer: cannot decide whether this smart account authorises")

// SmartAccount is a treasury whose authorisation is contract code.
//
// Weight arithmetic does not apply to it and is not attempted. A Soroban smart
// account decides for itself, in __check_auth, against authorisation entries
// carried beside the operations rather than signatures over the envelope hash —
// so counting signatures here would produce a number that means nothing and
// reads like it means something.
//
// This process still holds no key. What changes is only who answers the
// question: the contract, by simulation, instead of a threshold.
type SmartAccount struct {
	contract string
	check    AuthChecker
}

// AuthChecker asks the network whether a contract account authorises an
// envelope, by simulating it.
//
// An interface because the RPC client lands in a later phase, and because the
// answer must come from simulation against current ledger state — a cached or
// assumed answer is exactly the one an attacker would want.
type AuthChecker interface {
	// CheckAuth reports whether the envelope in req would be authorised by the
	// given contract account as the ledger currently stands.
	CheckAuth(ctx context.Context, contract string, req Request) (bool, error)
}

// NewSmartAccount returns a signer for a contract-controlled treasury.
//
// The checker is optional at construction so the router can be wired before
// Soroban RPC exists; Sign then refuses to answer rather than guessing.
func NewSmartAccount(contract string, check AuthChecker) *SmartAccount {
	return &SmartAccount{contract: contract, check: check}
}

// Address reports the contract that holds the funds.
func (s *SmartAccount) Address(context.Context) (string, error) {
	if !strkey.IsValidContractAddress(s.contract) {
		return "", fmt.Errorf("signer: %q is not a contract address", s.contract)
	}
	return s.contract, nil
}

// Sign contributes nothing and asks the contract whether it is satisfied.
func (s *SmartAccount) Sign(ctx context.Context, req Request) (Response, error) {
	if req.FeeBump != nil {
		return Response{}, errors.New(
			"signer: a fee bump is the operator's to sign, not a treasury's")
	}
	if !strkey.IsValidContractAddress(s.contract) {
		return Response{}, fmt.Errorf("signer: %q is not a contract address", s.contract)
	}
	if s.check == nil {
		return Response{}, fmt.Errorf("%w: %s has no simulator wired", ErrAuthUndecidable, s.contract)
	}

	ok, err := s.check.CheckAuth(ctx, s.contract, req)
	if err != nil {
		return Response{}, fmt.Errorf("signer: check auth for %s: %w", s.contract, err)
	}
	// Have and Need stay zero on purpose. There is no threshold to be part of
	// the way towards, and a caller that renders "1 of 2" here would be
	// inventing it.
	return Response{Tx: req.Tx, Complete: ok}, nil
}
