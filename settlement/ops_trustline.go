package settlement

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// Trustlines: what an account is willing to hold.
//
// An account cannot receive an issued asset without one, which makes a missing
// trustline the most common reason a perfectly good payment fails — and a
// failure that costs a whole approval cycle to discover.

// TrustRequest describes a trustline to open, change or close.
type TrustRequest struct {
	// Account holds the trustline. It sources the operation, so its signature
	// is what authorises the change.
	Account string
	Asset   txnbuild.CreditAsset
	// Limit is the most of the asset this account will hold. Zero closes the
	// trustline, which the network only permits at a zero balance.
	//
	// Nil means the maximum, which is what almost every caller wants and what a
	// limit of "as much as anyone sends me" means in practice.
	Limit *money.Stroops
}

// Trust returns the operation opening or changing a trustline.
func Trust(req TrustRequest) ([]txnbuild.Operation, error) {
	if !strkey.IsValidEd25519PublicKey(req.Account) {
		return nil, fmt.Errorf("settlement: %q is not a valid account", req.Account)
	}
	if req.Asset.Code == "" || req.Asset.Issuer == "" {
		return nil, errors.New("settlement: a trustline needs an issued asset")
	}
	if !strkey.IsValidEd25519PublicKey(req.Asset.Issuer) {
		return nil, fmt.Errorf("settlement: %q is not a valid issuer", req.Asset.Issuer)
	}
	if req.Account == req.Asset.Issuer {
		// An issuer holds an implicit trustline to its own asset, and the
		// network rejects the operation. Saying so here is more useful than
		// letting it fail on chain after an approval.
		return nil, errors.New("settlement: an issuer does not need a trustline to its own asset")
	}

	line, err := req.Asset.ToChangeTrustAsset()
	if err != nil {
		return nil, fmt.Errorf("settlement: asset as trustline: %w", err)
	}

	limit := txnbuild.MaxTrustlineLimit
	if req.Limit != nil {
		if req.Limit.Sign() < 0 {
			return nil, fmt.Errorf("settlement: trustline limit %s is negative", *req.Limit)
		}
		limit = req.Limit.String()
	}

	return []txnbuild.Operation{&txnbuild.ChangeTrust{
		SourceAccount: req.Account,
		Line:          line,
		Limit:         limit,
	}}, nil
}

// Untrust returns the operation closing a trustline.
//
// Separate from Trust rather than "a limit of zero", because the two are
// different intentions and only one of them can fail for a reason the caller
// needs to understand: the network refuses to close a trustline that still
// holds a balance, and a caller who meant "stop accepting more" would be
// confused by that.
func Untrust(account string, asset txnbuild.CreditAsset) ([]txnbuild.Operation, error) {
	zero := money.Stroops(0)
	return Trust(TrustRequest{Account: account, Asset: asset, Limit: &zero})
}
