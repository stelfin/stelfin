package settlement

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// Describing a transaction, so a person can check what they are about to sign.
//
// The rule this generalises is not "one operation". The old Describe refused
// anything with more than one because showing a payment while a second
// operation rode along is the attack it guarded against — but the valuable half
// of that check was never the count. It was: refuse to produce a signable
// artifact containing anything the renderer cannot fully describe.
//
// So multi-operation transactions are allowed and *undescribable* ones are not.
// A renderer that showed what it understood and quietly dropped the rest would
// be a safety downgrade wearing the clothes of a feature.
//
// Everything here is derived from the built transaction, never from the request
// that produced it. The server's job was to build it; being believed about what
// it built is a separate thing, and not one it gets — the browser re-derives the
// same description from the same XDR and compares.

// ErrIndescribable reports a transaction carrying something this renderer
// cannot fully show.
var ErrIndescribable = errors.New("settlement: transaction cannot be described")

// Field kinds. They tell a renderer how to present a value; they never change
// what it is.
const (
	KindAmount    = "amount"
	KindAsset     = "asset"
	KindAddress   = "address"
	KindNumber    = "number"
	KindPrice     = "price"
	KindFlag      = "flag"
	KindText      = "text"
	KindPredicate = "predicate"
	KindRaw       = "raw"
)

// Field is one labelled value of an operation.
type Field struct {
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Value is always a string.
	//
	// Never a JSON number. A float64 round trip is exactly what internal/money
	// exists to prevent, and JavaScript has no other numeric type — an amount
	// that renders differently on the two sides is a description that cannot be
	// compared, which is the same as no description at all.
	Value string `json:"value"`
}

// OpDescription is one operation, read back out of the envelope.
type OpDescription struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	// Source is the effective source: the operation's own, or the
	// transaction's where the operation does not name one.
	Source  string  `json:"source"`
	Summary string  `json:"summary"`
	Fields  []Field `json:"fields"`
}

// TxDescription is everything a signer is shown about a transaction.
type TxDescription struct {
	Kind     string `json:"kind"`
	Network  string `json:"network"`
	Hash     string `json:"hash"`
	Source   string `json:"source"`
	Sequence string `json:"sequence"`
	Fee      string `json:"fee"`
	MinTime  string `json:"min_time"`
	MaxTime  string `json:"max_time"`
	MemoType string `json:"memo_type"`
	Memo     string `json:"memo"`

	Operations []OpDescription `json:"operations"`
}

// DescribeTx reads a transaction back and reports everything it does.
//
// It refuses rather than approximating. An operation type this build does not
// know is not rendered as "unknown operation" and shown anyway — it produces
// ErrIndescribable, and nothing gets signed.
func (c *Client) DescribeTx(tx *txnbuild.Transaction) (*TxDescription, error) {
	if tx == nil {
		return nil, fmt.Errorf("%w: no transaction", ErrIndescribable)
	}

	hash, err := tx.HashHex(c.network)
	if err != nil {
		return nil, fmt.Errorf("settlement: hash transaction: %w", err)
	}

	d := &TxDescription{
		Kind:     "tx",
		Network:  c.network,
		Hash:     hash,
		Source:   tx.SourceAccount().AccountID,
		Sequence: strconv.FormatInt(tx.SourceAccount().Sequence, 10),
		// The envelope's actual fee field, not base fee times operations. They
		// agree for anything this builds, and a description must report what is
		// in the artifact rather than what should be.
		Fee: strconv.FormatInt(tx.MaxFee(), 10),
	}

	tb := tx.Timebounds()
	if tb.MinTime != 0 {
		d.MinTime = strconv.FormatInt(tb.MinTime, 10)
	}
	if tb.MaxTime != 0 {
		d.MaxTime = strconv.FormatInt(tb.MaxTime, 10)
	}

	d.MemoType, d.Memo, err = describeMemo(tx.Memo())
	if err != nil {
		return nil, err
	}

	ops := tx.Operations()
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: no operations", ErrIndescribable)
	}
	d.Operations = make([]OpDescription, 0, len(ops))
	for i, op := range ops {
		described, err := describeOp(i, op, d.Source)
		if err != nil {
			return nil, err
		}
		d.Operations = append(d.Operations, *described)
	}
	return d, nil
}

// DescribeFeeBump describes the inner transaction of a fee bump, noting who is
// paying.
//
// The inner transaction is what the signatures commit to; the outer envelope
// only says who covers the fee. Describing the inner one is therefore the
// honest thing to show, and the fee payer is a field rather than a separate
// screen.
func (c *Client) DescribeFeeBump(tx *txnbuild.FeeBumpTransaction) (*TxDescription, error) {
	if tx == nil {
		return nil, fmt.Errorf("%w: no transaction", ErrIndescribable)
	}
	inner := tx.InnerTransaction()
	if inner == nil {
		return nil, fmt.Errorf("%w: fee bump has no inner transaction", ErrIndescribable)
	}

	d, err := c.DescribeTx(inner)
	if err != nil {
		return nil, err
	}
	d.Kind = "fee_bump"
	hash, err := tx.HashHex(c.network)
	if err != nil {
		return nil, fmt.Errorf("settlement: hash fee bump: %w", err)
	}
	d.Hash = hash
	return d, nil
}

func describeMemo(m txnbuild.Memo) (kind, value string, err error) {
	switch v := m.(type) {
	case nil:
		return "none", "", nil
	case txnbuild.MemoText:
		return "text", string(v), nil
	case txnbuild.MemoID:
		return "id", strconv.FormatUint(uint64(v), 10), nil
	case txnbuild.MemoHash:
		return "hash", hexOf(v[:]), nil
	case txnbuild.MemoReturn:
		return "return", hexOf(v[:]), nil
	default:
		return "", "", fmt.Errorf("%w: memo type %T", ErrIndescribable, m)
	}
}

// describeOp renders one operation.
//
// The default case is a refusal, deliberately. Adding an operation type means
// adding a case here, and until someone does, a transaction carrying it cannot
// be signed through this system — which is the correct failure.
func describeOp(index int, op txnbuild.Operation, txSource string) (*OpDescription, error) {
	d := &OpDescription{Index: index, Source: txSource}

	switch o := op.(type) {
	case *txnbuild.Payment:
		d.Type = "payment"
		d.setSource(o.SourceAccount, txSource)
		amount, err := normalizeAmount(o.Amount)
		if err != nil {
			return nil, err
		}
		asset, err := describeAsset(o.Asset)
		if err != nil {
			return nil, err
		}
		d.Summary = fmt.Sprintf("Send %s %s to %s", amount, assetCode(asset), o.Destination)
		d.Fields = []Field{
			{Label: "amount", Kind: KindAmount, Value: amount},
			{Label: "asset", Kind: KindAsset, Value: asset},
			{Label: "destination", Kind: KindAddress, Value: o.Destination},
		}

	case *txnbuild.CreateAccount:
		d.Type = "create_account"
		d.setSource(o.SourceAccount, txSource)
		amount, err := normalizeAmount(o.Amount)
		if err != nil {
			return nil, err
		}
		d.Summary = fmt.Sprintf("Create account %s with %s XLM", o.Destination, amount)
		d.Fields = []Field{
			{Label: "destination", Kind: KindAddress, Value: o.Destination},
			{Label: "starting balance", Kind: KindAmount, Value: amount},
		}

	case *txnbuild.AccountMerge:
		d.Type = "account_merge"
		d.setSource(o.SourceAccount, txSource)
		d.Summary = fmt.Sprintf("Merge this account into %s, closing it", o.Destination)
		d.Fields = []Field{
			{Label: "destination", Kind: KindAddress, Value: o.Destination},
		}

	case *txnbuild.ChangeTrust:
		d.Type = "change_trust"
		d.setSource(o.SourceAccount, txSource)
		asset, err := describeChangeTrustAsset(o.Line)
		if err != nil {
			return nil, err
		}
		limit := o.Limit
		if limit == "" {
			limit = txnbuild.MaxTrustlineLimit
		}
		limit, err = normalizeAmount(limit)
		if err != nil {
			return nil, err
		}
		if limit == "0.0000000" {
			d.Summary = fmt.Sprintf("Remove the trustline for %s", assetCode(asset))
		} else {
			d.Summary = fmt.Sprintf("Trust %s up to %s", assetCode(asset), limit)
		}
		d.Fields = []Field{
			{Label: "asset", Kind: KindAsset, Value: asset},
			{Label: "limit", Kind: KindAmount, Value: limit},
		}

	case *txnbuild.BeginSponsoringFutureReserves:
		d.Type = "begin_sponsoring_future_reserves"
		d.setSource(o.SourceAccount, txSource)
		d.Summary = fmt.Sprintf("Pay the reserves %s is about to need", o.SponsoredID)
		d.Fields = []Field{
			{Label: "sponsored", Kind: KindAddress, Value: o.SponsoredID},
		}

	case *txnbuild.EndSponsoringFutureReserves:
		d.Type = "end_sponsoring_future_reserves"
		d.setSource(o.SourceAccount, txSource)
		d.Summary = "Stop sponsoring further reserves"

	case *txnbuild.SetOptions:
		d.Type = "set_options"
		d.setSource(o.SourceAccount, txSource)
		fields, summary, err := describeSetOptions(o)
		if err != nil {
			return nil, err
		}
		d.Summary = summary
		d.Fields = fields

	case *txnbuild.ManageSellOffer:
		d.Type = "manage_sell_offer"
		d.setSource(o.SourceAccount, txSource)
		fields, summary, err := describeOffer("Sell", o.Selling, o.Buying, o.Amount, o.Price, o.OfferID)
		if err != nil {
			return nil, err
		}
		d.Summary, d.Fields = summary, fields

	case *txnbuild.ManageBuyOffer:
		d.Type = "manage_buy_offer"
		d.setSource(o.SourceAccount, txSource)
		fields, summary, err := describeOffer("Buy", o.Selling, o.Buying, o.Amount, o.Price, o.OfferID)
		if err != nil {
			return nil, err
		}
		d.Summary, d.Fields = summary, fields

	case *txnbuild.CreatePassiveSellOffer:
		d.Type = "create_passive_sell_offer"
		d.setSource(o.SourceAccount, txSource)
		fields, summary, err := describeOffer("Passively sell", o.Selling, o.Buying, o.Amount, o.Price, 0)
		if err != nil {
			return nil, err
		}
		d.Summary, d.Fields = summary, fields

	case *txnbuild.PathPaymentStrictSend:
		d.Type = "path_payment_strict_send"
		d.setSource(o.SourceAccount, txSource)
		send, err := normalizeAmount(o.SendAmount)
		if err != nil {
			return nil, err
		}
		// DestMin, not a quote. The quote is a hope; this is the floor the
		// signature actually commits to, and it is what a person needs to see.
		destMin, err := normalizeAmount(o.DestMin)
		if err != nil {
			return nil, err
		}
		sendAsset, err := describeAsset(o.SendAsset)
		if err != nil {
			return nil, err
		}
		destAsset, err := describeAsset(o.DestAsset)
		if err != nil {
			return nil, err
		}
		path, err := describePath(o.Path)
		if err != nil {
			return nil, err
		}
		d.Summary = fmt.Sprintf("Send %s %s to %s, who receives at least %s %s",
			send, assetCode(sendAsset), o.Destination, destMin, assetCode(destAsset))
		d.Fields = []Field{
			{Label: "send amount", Kind: KindAmount, Value: send},
			{Label: "send asset", Kind: KindAsset, Value: sendAsset},
			{Label: "destination", Kind: KindAddress, Value: o.Destination},
			{Label: "destination asset", Kind: KindAsset, Value: destAsset},
			{Label: "minimum received", Kind: KindAmount, Value: destMin},
			{Label: "path", Kind: KindRaw, Value: path},
		}

	case *txnbuild.PathPaymentStrictReceive:
		d.Type = "path_payment_strict_receive"
		d.setSource(o.SourceAccount, txSource)
		sendMax, err := normalizeAmount(o.SendMax)
		if err != nil {
			return nil, err
		}
		destAmount, err := normalizeAmount(o.DestAmount)
		if err != nil {
			return nil, err
		}
		sendAsset, err := describeAsset(o.SendAsset)
		if err != nil {
			return nil, err
		}
		destAsset, err := describeAsset(o.DestAsset)
		if err != nil {
			return nil, err
		}
		path, err := describePath(o.Path)
		if err != nil {
			return nil, err
		}
		d.Summary = fmt.Sprintf("Send %s at most %s %s so they receive %s %s",
			o.Destination, sendMax, assetCode(sendAsset), destAmount, assetCode(destAsset))
		d.Fields = []Field{
			{Label: "maximum sent", Kind: KindAmount, Value: sendMax},
			{Label: "send asset", Kind: KindAsset, Value: sendAsset},
			{Label: "destination", Kind: KindAddress, Value: o.Destination},
			{Label: "destination asset", Kind: KindAsset, Value: destAsset},
			{Label: "received", Kind: KindAmount, Value: destAmount},
			{Label: "path", Kind: KindRaw, Value: path},
		}

	case *txnbuild.BumpSequence:
		d.Type = "bump_sequence"
		d.setSource(o.SourceAccount, txSource)
		d.Summary = fmt.Sprintf("Bump the sequence number to %d", o.BumpTo)
		d.Fields = []Field{
			{Label: "bump to", Kind: KindNumber, Value: strconv.FormatInt(o.BumpTo, 10)},
		}

	case *txnbuild.ManageData:
		d.Type = "manage_data"
		d.setSource(o.SourceAccount, txSource)
		if o.Value == nil {
			d.Summary = fmt.Sprintf("Delete the data entry %q", o.Name)
			d.Fields = []Field{{Label: "name", Kind: KindText, Value: o.Name}}
		} else {
			d.Summary = fmt.Sprintf("Set the data entry %q", o.Name)
			d.Fields = []Field{
				{Label: "name", Kind: KindText, Value: o.Name},
				// Hex, not the raw bytes: the value is arbitrary and may not be
				// text at all, and rendering it as text would be a way to smuggle
				// something that looks like a different field.
				{Label: "value", Kind: KindRaw, Value: hexOf(o.Value)},
			}
		}

	case *txnbuild.InvokeHostFunction:
		d.Type = "invoke_contract"
		d.setSource(o.SourceAccount, txSource)
		fields, summary, err := describeInvoke(o)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
		}
		d.Fields = fields
		d.Summary = summary

	default:
		// The refusal that carries the whole guarantee. An operation this
		// cannot render exactly is refused rather than summarised, because a
		// description that skipped what it did not understand would let an
		// operation ride along unmentioned — which is the whole attack.
		return nil, fmt.Errorf("%w: operation %d is a %T", ErrIndescribable, index, op)
	}

	return d, nil
}

// setSource records the operation's effective source.
func (d *OpDescription) setSource(opSource, txSource string) {
	if opSource != "" {
		d.Source = opSource
		return
	}
	d.Source = txSource
}

func describeOffer(
	verb string, selling, buying txnbuild.Asset, amount string, price xdr.Price, offerID int64,
) ([]Field, string, error) {
	normalized, err := normalizeAmount(amount)
	if err != nil {
		return nil, "", err
	}
	sell, err := describeAsset(selling)
	if err != nil {
		return nil, "", err
	}
	buy, err := describeAsset(buying)
	if err != nil {
		return nil, "", err
	}
	// A rational, never a decimal. Dividing to render it would introduce the
	// one thing this whole layer exists to keep out.
	rate := fmt.Sprintf("%d/%d", price.N, price.D)

	summary := fmt.Sprintf("%s %s %s for %s at %s",
		verb, normalized, assetCode(sell), assetCode(buy), rate)
	if normalized == "0.0000000" {
		summary = fmt.Sprintf("Cancel offer %d", offerID)
	}

	return []Field{
		{Label: "selling", Kind: KindAsset, Value: sell},
		{Label: "buying", Kind: KindAsset, Value: buy},
		{Label: "amount", Kind: KindAmount, Value: normalized},
		{Label: "price", Kind: KindPrice, Value: rate},
		{Label: "offer", Kind: KindNumber, Value: strconv.FormatInt(offerID, 10)},
	}, summary, nil
}

// describeSetOptions renders a signer and threshold change.
//
// The one operation that can make an account permanently unusable, so every
// part of it is shown even when unchanged is the interesting case: a threshold
// left alone next to a master weight set to zero is exactly the shape of a
// lockout, and a renderer that omitted the unchanged half would hide it.
func describeSetOptions(o *txnbuild.SetOptions) ([]Field, string, error) {
	var fields []Field
	var changes []string

	if o.MasterWeight != nil {
		fields = append(fields, Field{
			Label: "master weight", Kind: KindNumber,
			Value: strconv.FormatUint(uint64(*o.MasterWeight), 10),
		})
		changes = append(changes, "master weight")
	}
	// Thresholds in a fixed order. A map iteration would make the canonical
	// form differ between runs, which is the one thing it may not do.
	for _, th := range []struct {
		label string
		value *txnbuild.Threshold
	}{
		{"low threshold", o.LowThreshold},
		{"medium threshold", o.MediumThreshold},
		{"high threshold", o.HighThreshold},
	} {
		if th.value == nil {
			continue
		}
		fields = append(fields, Field{
			Label: th.label, Kind: KindNumber,
			Value: strconv.FormatUint(uint64(*th.value), 10),
		})
		changes = append(changes, th.label)
	}

	if o.Signer != nil {
		fields = append(fields,
			Field{Label: "signer", Kind: KindAddress, Value: o.Signer.Address},
			Field{
				Label: "signer weight", Kind: KindNumber,
				Value: strconv.FormatUint(uint64(o.Signer.Weight), 10),
			},
		)
		if o.Signer.Weight == 0 {
			changes = append(changes, "remove a signer")
		} else {
			changes = append(changes, "add or change a signer")
		}
	}
	if o.HomeDomain != nil {
		fields = append(fields, Field{Label: "home domain", Kind: KindText, Value: *o.HomeDomain})
		changes = append(changes, "home domain")
	}
	if o.InflationDestination != nil {
		fields = append(fields, Field{
			Label: "inflation destination", Kind: KindAddress, Value: *o.InflationDestination,
		})
		changes = append(changes, "inflation destination")
	}
	for _, f := range o.SetFlags {
		fields = append(fields, Field{Label: "set flag", Kind: KindFlag, Value: flagName(f)})
		changes = append(changes, "set "+flagName(f))
	}
	for _, f := range o.ClearFlags {
		fields = append(fields, Field{Label: "clear flag", Kind: KindFlag, Value: flagName(f)})
		changes = append(changes, "clear "+flagName(f))
	}

	if len(fields) == 0 {
		return nil, "", fmt.Errorf("%w: set_options changes nothing", ErrIndescribable)
	}
	return fields, "Change account settings: " + strings.Join(changes, ", "), nil
}

func flagName(f txnbuild.AccountFlag) string {
	switch f {
	case txnbuild.AuthRequired:
		return "auth required"
	case txnbuild.AuthRevocable:
		return "auth revocable"
	case txnbuild.AuthImmutable:
		return "auth immutable"
	case txnbuild.AuthClawbackEnabled:
		return "clawback enabled"
	default:
		return "flag " + strconv.FormatUint(uint64(f), 10)
	}
}

// describeAsset renders an asset canonically.
func describeAsset(a txnbuild.Asset) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: operation has no asset", ErrIndescribable)
	}
	if a.IsNative() {
		return "native", nil
	}
	code, issuer := a.GetCode(), a.GetIssuer()
	if code == "" || issuer == "" {
		return "", fmt.Errorf("%w: asset has no code or issuer", ErrIndescribable)
	}
	return code + ":" + issuer, nil
}

func describeChangeTrustAsset(a txnbuild.ChangeTrustAsset) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: change_trust has no asset", ErrIndescribable)
	}
	if a.IsNative() {
		// Trusting the native asset is meaningless and cannot be built, so
		// seeing one means the envelope is not what it claims to be.
		return "", fmt.Errorf("%w: change_trust on the native asset", ErrIndescribable)
	}
	// A liquidity-pool trustline is a different shape entirely, and rendering
	// it as an asset pair would misdescribe what is being trusted.
	if _, isPool := a.GetLiquidityPoolID(); isPool {
		return "", fmt.Errorf("%w: change_trust on a liquidity pool share", ErrIndescribable)
	}
	if _, isPool := a.GetLiquidityPoolParameters(); isPool {
		return "", fmt.Errorf("%w: change_trust on a liquidity pool share", ErrIndescribable)
	}
	code, issuer := a.GetCode(), a.GetIssuer()
	if code == "" || issuer == "" {
		return "", fmt.Errorf("%w: trustline asset has no code or issuer", ErrIndescribable)
	}
	return code + ":" + issuer, nil
}

func describePath(path []txnbuild.Asset) (string, error) {
	if len(path) == 0 {
		return "direct", nil
	}
	parts := make([]string, len(path))
	for i, a := range path {
		rendered, err := describeAsset(a)
		if err != nil {
			return "", err
		}
		parts[i] = rendered
	}
	return strings.Join(parts, " → "), nil
}

// assetCode is the short name for a summary line. The canonical field keeps the
// issuer; a sentence does not need it.
func assetCode(asset string) string {
	if asset == "native" {
		return "XLM"
	}
	code, _, _ := strings.Cut(asset, ":")
	return code
}

// normalizeAmount re-renders an amount in canonical form.
//
// "100" and "100.0000000" are the same amount and must produce the same
// description, or two correct implementations would disagree about a
// transaction they both read correctly.
func normalizeAmount(amount string) (string, error) {
	if amount == "" {
		return "", fmt.Errorf("%w: missing amount", ErrIndescribable)
	}
	parsed, err := money.Parse(amount)
	if err != nil {
		return "", fmt.Errorf("%w: unreadable amount %q: %v", ErrIndescribable, amount, err)
	}
	return parsed.String(), nil
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// SinglePayment reports the description as a single payment, if that is all it
// is.
//
// The bridge for callers written when a transaction could only ever be one
// payment. They get the same answer through the general renderer, which means
// there is one implementation of "what does this transaction do" rather than
// two that can drift.
//
// It is as strict as the old Describe was, and for the same reason: a caller
// asking for "the payment" must not be handed the first of several while the
// rest ride along unmentioned.
func (d *TxDescription) SinglePayment() (*PaymentDescription, bool) {
	if len(d.Operations) != 1 || d.Operations[0].Type != "payment" {
		return nil, false
	}
	op := d.Operations[0]

	var amount, asset, destination string
	for _, f := range op.Fields {
		switch f.Label {
		case "amount":
			amount = f.Value
		case "asset":
			asset = f.Value
		case "destination":
			destination = f.Value
		}
	}

	parsed, err := money.Parse(amount)
	if err != nil || parsed.Sign() <= 0 {
		return nil, false
	}

	out := &PaymentDescription{
		From:   op.Source,
		To:     destination,
		Amount: parsed,
		Hash:   d.Hash,
	}
	if asset == "native" {
		out.AssetNative = true
		out.AssetCode = "XLM"
	} else {
		code, issuer, ok := strings.Cut(asset, ":")
		if !ok {
			return nil, false
		}
		out.AssetCode, out.AssetIssuer = code, issuer
	}
	return out, true
}
