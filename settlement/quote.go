package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
)

// What a swap would bring, asked of the network before anyone is asked to sign.
//
// A swap's risk is one number: the least it may deliver. Nobody can choose that
// number without knowing what the market offers now, and a proposal built on a
// guess either fails on chain, costing its signers a round of signing, or
// accepts far less than the asset is worth. So the market is asked first.

// ErrNoPath reports that nothing on the network will exchange the one asset for
// the other right now.
//
// An ordinary answer, and on testnet a common one: an order book exists only
// where somebody has placed an order. It is not a failure of the request.
var ErrNoPath = errors.New("settlement: nobody is offering that exchange right now")

// ErrNoPathFinder reports a client whose Horizon transport cannot look for
// paths. Only a test double can be one.
var ErrNoPathFinder = errors.New("settlement: this client cannot ask the network for a quote")

// pathFinder is the part of a Horizon client that finds payment paths. Kept
// apart from HorizonAPI so that the doubles which test building and submitting
// need not also implement a market.
type pathFinder interface {
	StrictSendPaths(horizonclient.StrictSendPathsRequest) (horizon.PathsPage, error)
}

// Quote is what sending an amount of one asset would deliver of another.
type Quote struct {
	// Receive is what the best path delivers at this moment. It is a reading,
	// not a promise: the book can move before a proposal is signed.
	Receive money.Stroops
	// Path is the assets the exchange passes through. Empty means directly.
	Path []txnbuild.Asset
}

// QuoteStrictSend asks what sending amount of send would deliver of dest.
func (c *Client) QuoteStrictSend(
	_ context.Context, send txnbuild.Asset, amount money.Stroops, dest txnbuild.Asset,
) (Quote, error) {
	finder, ok := c.horizon.(pathFinder)
	if !ok {
		return Quote{}, ErrNoPathFinder
	}
	if send == nil || dest == nil {
		return Quote{}, errors.New("settlement: a quote needs both assets")
	}
	if amount.Sign() <= 0 {
		return Quote{}, fmt.Errorf("settlement: quote amount %s is not positive", amount)
	}

	req := horizonclient.StrictSendPathsRequest{
		SourceAmount:      amount.String(),
		DestinationAssets: assetParam(dest),
	}
	if send.IsNative() {
		req.SourceAssetType = horizonclient.AssetTypeNative
	} else {
		req.SourceAssetType = creditType(send.GetCode())
		req.SourceAssetCode = send.GetCode()
		req.SourceAssetIssuer = send.GetIssuer()
	}

	page, err := finder.StrictSendPaths(req)
	if err != nil {
		return Quote{}, fmt.Errorf("settlement: ask for a quote: %w", err)
	}

	// Horizon lists the best first, and that is not relied on: the best is
	// whichever delivers most, and reading every record costs nothing.
	var best Quote
	found := false
	for _, record := range page.Embedded.Records {
		got, err := money.Parse(record.DestinationAmount)
		if err != nil || got.Sign() <= 0 {
			continue
		}
		if found && got <= best.Receive {
			continue
		}
		path := make([]txnbuild.Asset, 0, len(record.Path))
		for _, hop := range record.Path {
			if hop.Type == "native" {
				path = append(path, txnbuild.NativeAsset{})
				continue
			}
			path = append(path, txnbuild.CreditAsset{Code: hop.Code, Issuer: hop.Issuer})
		}
		best, found = Quote{Receive: got, Path: path}, true
	}
	if !found {
		return Quote{}, ErrNoPath
	}
	return best, nil
}

// assetParam is an asset as Horizon's path endpoints name it: "native", or
// "CODE:ISSUER".
func assetParam(a txnbuild.Asset) string {
	if a.IsNative() {
		return "native"
	}
	return a.GetCode() + ":" + a.GetIssuer()
}

func creditType(code string) horizonclient.AssetType {
	if len(code) <= 4 {
		return horizonclient.AssetType4
	}
	return horizonclient.AssetType12
}
