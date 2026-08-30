package settlement

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// Trading on Stellar's own order book.
//
// Two things here are easy to get wrong and expensive to get wrong.
//
// A price is a rational, not a decimal. Stellar stores it as a numerator and a
// denominator, and it means exactly what those two integers say. Rendering it
// as a decimal introduces rounding into the one place rounding must not go, and
// parsing a decimal back into a rational guesses at what someone meant. So a
// price is given as a pair here and stays a pair everywhere.
//
// And a swap has no fixed outcome. The order book moves between building a
// transaction and its inclusion, so what the signature commits to is not a
// quote — it is a bound. A caller must state the worst outcome they will accept,
// and that bound is what gets shown, because the quote is a hope and the bound
// is the promise.

// ErrNoSlippageBound reports a swap with no worst-case bound.
var ErrNoSlippageBound = errors.New("settlement: a swap needs an explicit worst-case bound")

// Price is an exact rational.
type Price struct {
	N, D int32
}

// NewPrice returns a price, refusing anything that is not a usable rational.
func NewPrice(n, d int32) (Price, error) {
	if n <= 0 || d <= 0 {
		return Price{}, fmt.Errorf("settlement: price %d/%d must have positive terms", n, d)
	}
	return Price{N: n, D: d}, nil
}

func (p Price) xdr() xdr.Price { return xdr.Price{N: xdr.Int32(p.N), D: xdr.Int32(p.D)} }

// String renders the rational, never a decimal.
func (p Price) String() string { return fmt.Sprintf("%d/%d", p.N, p.D) }

// Cmp orders two prices without leaving exact arithmetic.
//
// a/b < c/d exactly when a*d < c*b, for positive terms. big.Int rather than
// int64 because the cross-multiplication of two int32 pairs is comfortably
// within int64 — and relying on that is the kind of thing that stays true until
// the day the types widen.
func (p Price) Cmp(other Price) int {
	left := new(big.Int).Mul(big.NewInt(int64(p.N)), big.NewInt(int64(other.D)))
	right := new(big.Int).Mul(big.NewInt(int64(other.N)), big.NewInt(int64(p.D)))
	return left.Cmp(right)
}

// OfferRequest describes an order to place, change or cancel.
type OfferRequest struct {
	// Account places the offer and sources the operation.
	Account         string
	Selling, Buying txnbuild.Asset
	// Amount of the selling asset. Zero cancels an existing offer.
	Amount money.Stroops
	Price  Price
	// OfferID is zero for a new offer, or an existing id to change or cancel.
	OfferID int64
	// Buy places a buy offer, where Amount is of the buying asset instead.
	Buy bool
}

// Offer returns the operation placing, changing or cancelling an order.
func Offer(req OfferRequest) ([]txnbuild.Operation, error) {
	if !strkey.IsValidEd25519PublicKey(req.Account) {
		return nil, fmt.Errorf("settlement: %q is not a valid account", req.Account)
	}
	if req.Selling == nil || req.Buying == nil {
		return nil, errors.New("settlement: an offer needs both assets")
	}
	selling, err := describeAsset(req.Selling)
	if err != nil {
		return nil, err
	}
	buying, err := describeAsset(req.Buying)
	if err != nil {
		return nil, err
	}
	if selling == buying {
		return nil, errors.New("settlement: an offer cannot sell an asset for itself")
	}

	cancelling := req.Amount.IsZero()
	if cancelling && req.OfferID == 0 {
		return nil, errors.New("settlement: cancelling needs the id of the offer to cancel")
	}
	if req.Amount.Sign() < 0 {
		return nil, fmt.Errorf("settlement: offer amount %s is negative", req.Amount)
	}
	if !cancelling && (req.Price.N <= 0 || req.Price.D <= 0) {
		return nil, fmt.Errorf("settlement: price %s must have positive terms", req.Price)
	}

	price := req.Price
	if cancelling {
		// The network ignores the price of a cancellation, but the builder
		// still validates it, so give it something well-formed rather than
		// making every caller remember this.
		price = Price{N: 1, D: 1}
	}

	if req.Buy {
		return []txnbuild.Operation{&txnbuild.ManageBuyOffer{
			SourceAccount: req.Account,
			Selling:       req.Selling, Buying: req.Buying,
			Amount: req.Amount.String(), Price: price.xdr(), OfferID: req.OfferID,
		}}, nil
	}
	return []txnbuild.Operation{&txnbuild.ManageSellOffer{
		SourceAccount: req.Account,
		Selling:       req.Selling, Buying: req.Buying,
		Amount: req.Amount.String(), Price: price.xdr(), OfferID: req.OfferID,
	}}, nil
}

// SwapRequest describes a path payment.
type SwapRequest struct {
	// From sources the operation and pays.
	From string
	// To receives. The same account as From is an ordinary way to swap one
	// asset for another without involving anyone else.
	To              string
	SendAsset       txnbuild.Asset
	DestAsset       txnbuild.Asset
	SendAmount      money.Stroops
	MinimumReceived money.Stroops
	// Path is the intermediate assets. Empty means direct.
	Path []txnbuild.Asset
}

// Swap returns a path payment with an explicit floor on what arrives.
//
// MinimumReceived is required and has no default. A default would be a number
// this package invented on someone else's behalf, and the number is the entire
// risk: it is the difference between "I accepted a worse rate than I hoped" and
// "the order book was drained and I accepted almost nothing".
func Swap(req SwapRequest) ([]txnbuild.Operation, error) {
	switch {
	case !strkey.IsValidEd25519PublicKey(req.From):
		return nil, fmt.Errorf("settlement: %q is not a valid sending account", req.From)
	case !strkey.IsValidEd25519PublicKey(req.To):
		return nil, fmt.Errorf("settlement: %q is not a valid destination", req.To)
	case req.SendAsset == nil || req.DestAsset == nil:
		return nil, errors.New("settlement: a swap needs both assets")
	case req.SendAmount.Sign() <= 0:
		return nil, fmt.Errorf("settlement: send amount %s is not positive", req.SendAmount)
	case req.MinimumReceived.Sign() <= 0:
		return nil, fmt.Errorf("%w: minimum received is %s", ErrNoSlippageBound, req.MinimumReceived)
	}

	return []txnbuild.Operation{&txnbuild.PathPaymentStrictSend{
		SourceAccount: req.From,
		SendAsset:     req.SendAsset,
		SendAmount:    req.SendAmount.String(),
		Destination:   req.To,
		DestAsset:     req.DestAsset,
		DestMin:       req.MinimumReceived.String(),
		Path:          req.Path,
	}}, nil
}

// SlippageBound computes the worst acceptable outcome from a quote.
//
// Given what the book says now and how much worse a caller will tolerate, in
// basis points. Integer arithmetic throughout: quote * (10000 - bps) / 10000,
// truncated, so the bound is never rounded *up* into something the caller did
// not agree to.
//
// A helper, not a default. Somebody still has to choose the tolerance, and
// choosing it is the part that matters.
func SlippageBound(quote money.Stroops, toleranceBps int64) (money.Stroops, error) {
	switch {
	case quote.Sign() <= 0:
		return 0, fmt.Errorf("settlement: quote %s is not positive", quote)
	case toleranceBps < 0 || toleranceBps >= 10_000:
		return 0, fmt.Errorf(
			"settlement: slippage tolerance %d basis points is not between 0 and 9999", toleranceBps)
	}

	// big.Int because quote * 10000 overflows int64 for large amounts, and an
	// overflow here would produce a bound far below what was intended.
	scaled := new(big.Int).Mul(big.NewInt(int64(quote)), big.NewInt(10_000-toleranceBps))
	scaled.Div(scaled, big.NewInt(10_000))
	if !scaled.IsInt64() {
		return 0, fmt.Errorf("settlement: slippage bound for %s does not fit", quote)
	}
	bound := money.Stroops(scaled.Int64())
	if bound.Sign() <= 0 {
		return 0, fmt.Errorf(
			"settlement: a %d basis point tolerance on %s leaves nothing", toleranceBps, quote)
	}
	return bound, nil
}
