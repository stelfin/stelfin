package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/settlement"
)

// What an account holds, read from the network, and how an asset is named.
//
// Read live and never from the ledger. The ledger is an index of what this
// deployment has seen the chain do; a balance is a fact about the chain now,
// and an account anybody can pay has a balance this deployment did not have to
// be involved in to change.

// RefusalError reports something this deployment will not put to signers
// because it could not land, in words for the person who asked.
//
// Signatures are the one thing a treasury's signers have to give, and finding
// out at submission that a trustline was already there, or that a balance was
// not, spends a round of them for nothing. These are found first.
type RefusalError struct {
	Reason string
}

func (e *RefusalError) Error() string { return "api: " + e.Reason }

func refusef(format string, args ...any) error {
	return &RefusalError{Reason: fmt.Sprintf(format, args...)}
}

// Holding is one asset an account holds.
type Holding struct {
	Asset   txnbuild.Asset
	Balance money.Stroops
	// Limit is the most the account will hold. Zero for XLM, which has none.
	Limit money.Stroops
	// Selling is what open offers have already promised away.
	Selling money.Stroops
	// Authorized is false when the issuer has not let this account hold it.
	Authorized bool
}

// AccountState is an account as the network has it now.
type AccountState struct {
	Address  string
	Exists   bool
	Holdings []Holding
	// Spare is the XLM the account can part with: what it holds, less what the
	// network requires it to keep and what its offers have promised.
	Spare money.Stroops
}

// holding returns the account's line for an asset.
func (a AccountState) holding(asset txnbuild.Asset) (Holding, bool) {
	for _, h := range a.Holdings {
		if sameAsset(h.Asset, asset) {
			return h, true
		}
	}
	return Holding{}, false
}

func sameAsset(a, b txnbuild.Asset) bool {
	if a == nil || b == nil {
		return false
	}
	if a.IsNative() || b.IsNative() {
		return a.IsNative() && b.IsNative()
	}
	return a.GetCode() == b.GetCode() && a.GetIssuer() == b.GetIssuer()
}

// accountState reads an account from the network. One that does not exist is
// not an error: it is an account nobody has funded, which is worth saying.
func (s *Service) accountState(ctx context.Context, address string) (AccountState, error) {
	account, err := s.settle.LoadAccount(ctx, address)
	if err != nil {
		var problem *horizonclient.Error
		if errors.As(err, &problem) && problem.Problem.Status == http.StatusNotFound {
			return AccountState{Address: address}, nil
		}
		return AccountState{}, err
	}
	return stateOf(address, account)
}

func stateOf(address string, account *horizon.Account) (AccountState, error) {
	out := AccountState{Address: address, Exists: true}
	var native, nativeSelling money.Stroops
	for _, b := range account.Balances {
		balance, err := money.Parse(b.Balance)
		if err != nil {
			return AccountState{}, fmt.Errorf("api: read a balance of %s: %w", address, err)
		}
		selling, _ := money.Parse(orZero(b.SellingLiabilities))

		switch b.Asset.Type {
		case "native":
			native, nativeSelling = balance, selling
			out.Holdings = append(out.Holdings, Holding{
				Asset: txnbuild.NativeAsset{}, Balance: balance, Selling: selling, Authorized: true,
			})
		case "credit_alphanum4", "credit_alphanum12":
			limit, _ := money.Parse(orZero(b.Limit))
			out.Holdings = append(out.Holdings, Holding{
				Asset:      txnbuild.CreditAsset{Code: b.Asset.Code, Issuer: b.Asset.Issuer},
				Balance:    balance,
				Limit:      limit,
				Selling:    selling,
				Authorized: b.IsAuthorized == nil || *b.IsAuthorized,
			})
		}
		// Liquidity pool shares are left out. Nothing here creates them, and a
		// line this cannot name is better absent than half-described.
	}

	// What the network makes the account keep: two reserves for existing, and
	// one for each thing it owns or sponsors for somebody else, less whatever
	// somebody else is sponsoring for it.
	entries := int64(2) + int64(account.SubentryCount) + int64(account.NumSponsoring) - int64(account.NumSponsored)
	if entries < 0 {
		entries = 0
	}
	out.Spare = native - money.Stroops(entries)*settlement.BaseReserve - nativeSelling
	if out.Spare < 0 {
		out.Spare = 0
	}
	return out, nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// parseAsset reads an asset as somebody types it.
//
//	XLM            the network's own asset
//	USDC           the deployment's asset, or one the account already holds
//	EURC:GABC…     any issued asset, named in full
//
// A bare code is accepted only where it can mean one thing. A code is a label
// its issuer chose and anybody can issue another asset with the same one, so
// "USDC" by itself names whichever USDC this deployment was configured with or
// the account already trusts — never one looked up by name. Anything else has
// to be written with its issuer, which is the only part of an asset's name
// that cannot be imitated.
func (s *Service) parseAsset(text string, held []Holding) (txnbuild.Asset, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, refusef("no asset was named")
	}
	if strings.EqualFold(text, "XLM") || strings.EqualFold(text, "native") {
		return txnbuild.NativeAsset{}, nil
	}

	if code, issuer, full := strings.Cut(text, ":"); full {
		code, issuer = strings.TrimSpace(code), strings.TrimSpace(issuer)
		if !validCode(code) {
			return nil, refusef("%q is not an asset code: one to twelve letters and digits", code)
		}
		if !strkey.IsValidEd25519PublicKey(issuer) {
			return nil, refusef("%q is not an issuer: it should be the G… account that issues %s", issuer, code)
		}
		return txnbuild.CreditAsset{Code: code, Issuer: issuer}, nil
	}

	if !validCode(text) {
		return nil, refusef("%q is not an asset: write XLM, or CODE:ISSUER", text)
	}
	if configured, ok := s.cfg.Asset.(txnbuild.CreditAsset); ok && strings.EqualFold(text, configured.Code) {
		return configured, nil
	}
	var match []txnbuild.Asset
	for _, h := range held {
		if !h.Asset.IsNative() && strings.EqualFold(h.Asset.GetCode(), text) {
			match = append(match, h.Asset)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return nil, refusef("I don't know which %s you mean. Anybody can issue an asset with that "+
			"code, so name its issuer: %s:G…", strings.ToUpper(text), strings.ToUpper(text))
	default:
		return nil, refusef("the treasury holds more than one asset called %s. Say which, with "+
			"its issuer: %s:G…", strings.ToUpper(text), strings.ToUpper(text))
	}
}

func validCode(code string) bool {
	if len(code) < 1 || len(code) > 12 {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// AssetName is an asset as a reply names it: XLM, the deployment's own asset by
// its code, and anything else in full. In full because the code alone would
// read the same for two different assets, and a signer should never have to
// wonder which.
func (s *Service) AssetName(a txnbuild.Asset) string {
	if a == nil {
		return ""
	}
	if a.IsNative() {
		return "XLM"
	}
	if configured, ok := s.cfg.Asset.(txnbuild.CreditAsset); ok && sameAsset(a, configured) {
		return configured.Code
	}
	return a.GetCode() + ":" + a.GetIssuer()
}
