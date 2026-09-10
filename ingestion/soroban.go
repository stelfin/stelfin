package ingestion

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// Watching money move inside contracts.
//
// A DAO whose treasury is a Soroban contract produces no Horizon payment
// operations at all. Its balance changes through `transfer` events emitted by a
// token contract, and a deployment watching only Horizon would report that
// treasury as permanently empty — not visibly broken, just wrong, which is the
// worse failure.
//
// The events watched are the token interface's, not the Stellar Asset
// Contract's specifically. A SAC emits them and so does any contract
// implementing the same interface, and there is no reason to see one and not
// the other: both move a DAO's money.

// ErrRetentionGap reports that the RPC's history no longer reaches this
// source's position.
//
// The failure this whole file is arranged around. An RPC keeps a bounded window
// of events — days, not years — and a source that has been stopped longer than
// that window cannot catch up: the events in between are gone from that
// endpoint permanently.
//
// So it refuses to advance. Skipping forward to whatever the RPC still has
// would leave the ledger missing transfers with nothing recording that they
// were missed, and every balance downstream quietly wrong. Stopping is loud,
// recoverable, and puts a person in the loop — which is the correct outcome
// when the alternative is silent, permanent divergence.
var ErrRetentionGap = errors.New(
	"ingestion: the RPC no longer retains events from this position")

// ErrAmountTooLarge reports a transfer that does not fit the ledger's integer.
//
// Token amounts are i128 and this ledger's are int64 stroops. A value that
// exceeds int64 is not something to truncate — truncation would record a
// different, smaller payment and every check downstream would agree with it.
var ErrAmountTooLarge = errors.New("ingestion: transfer amount does not fit in stroops")

// EventsAPI is the slice of Soroban RPC this source needs.
type EventsAPI interface {
	GetEvents(
		ctx context.Context, req protocol.GetEventsRequest,
	) (protocol.GetEventsResponse, error)
}

// TransferSource reads token transfer events from Soroban RPC.
type TransferSource struct {
	api  EventsAPI
	name string
	// startLedger is where to begin when there is no cursor yet. Zero means
	// "the oldest the RPC still has", which is the only honest default: an
	// endpoint cannot serve what it has forgotten, and pretending to start at
	// ledger one would fail on the first call.
	startLedger uint32
}

// NewTransferSource returns a source reading token transfer events.
func NewTransferSource(api EventsAPI, name string, startLedger uint32) (*TransferSource, error) {
	if api == nil {
		return nil, errors.New("ingestion: transfer source needs a Soroban RPC client")
	}
	if name == "" {
		return nil, errors.New("ingestion: a source needs a name to track its cursor under")
	}
	return &TransferSource{api: api, name: name, startLedger: startLedger}, nil
}

// Name is the cursor row this source tracks.
func (s *TransferSource) Name() string { return s.name }

// Fetch returns one page of transfer events, translated.
func (s *TransferSource) Fetch(
	ctx context.Context, cursor string, limit uint,
) ([]Record, error) {
	req := protocol.GetEventsRequest{
		Filters: []protocol.EventFilter{{
			EventType: protocol.EventTypeSet{protocol.EventTypeContract: nil},
			// Topic zero is the function name; the rest are the transfer's own
			// arguments and are matched by anything. Filtering at the RPC
			// rather than locally matters: a busy network emits far more events
			// than this deployment could page through.
			Topics: []protocol.TopicFilter{{
				{ScVal: symbolVal("transfer")},
				{Wildcard: wildcard()},
				{Wildcard: wildcard()},
				{Wildcard: wildcard()},
			}},
		}},
		Pagination: &protocol.PaginationOptions{Limit: limit},
	}

	// A cursor and a ledger range are mutually exclusive at the RPC, so this is
	// one or the other rather than both.
	if cursor == "" {
		req.StartLedger = s.startLedger
	} else {
		parsed, err := protocol.ParseCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("ingestion: unreadable event cursor %q: %w", cursor, err)
		}
		req.Pagination.Cursor = &parsed
	}

	resp, err := s.api.GetEvents(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("ingestion: fetch events after %q: %w", cursor, err)
	}

	if err := s.checkRetention(cursor, resp); err != nil {
		return nil, err
	}

	// No cursor and no configured start: adopt the oldest ledger the RPC still
	// has, and say so by refusing rather than silently beginning mid-history.
	if cursor == "" && s.startLedger == 0 && resp.OldestLedger > 0 {
		return nil, fmt.Errorf(
			"%w: no starting point is configured, and this endpoint's history "+
				"begins at ledger %d. Starting there would silently skip everything "+
				"before it; set a start ledger deliberately",
			ErrRetentionGap, resp.OldestLedger)
	}

	out := make([]Record, 0, len(resp.Events))
	for _, event := range resp.Events {
		record, err := transferRecord(event)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// checkRetention refuses to continue past a hole.
//
// The RPC reports the oldest ledger it still holds. If that is past where this
// source left off, the events in between are gone — and the only two options
// are to skip them or to stop. Skipping is what makes a ledger quietly wrong,
// so this stops.
func (s *TransferSource) checkRetention(cursor string, resp protocol.GetEventsResponse) error {
	if cursor == "" || resp.OldestLedger == 0 {
		return nil
	}
	parsed, err := protocol.ParseCursor(cursor)
	if err != nil {
		return fmt.Errorf("ingestion: unreadable event cursor %q: %w", cursor, err)
	}

	// The cursor names the last event consumed, so the next one this source
	// needs is in that ledger or after it. A retained history starting at the
	// cursor's own ledger is fine; one starting later has swallowed events.
	if resp.OldestLedger > parsed.Ledger {
		return fmt.Errorf(
			"%w: last read ledger %d, and its history now begins at ledger %d. "+
				"Ingestion is stopped rather than skipping ahead, because the "+
				"transfers in between would be missing with nothing recording that "+
				"they are missing. Backfill from an archive, or reset this source's "+
				"cursor deliberately",
			ErrRetentionGap, parsed.Ledger, resp.OldestLedger)
	}
	return nil
}

// transferRecord translates one token transfer event.
//
// The token interface's event is (topics, data) where topics are
// [symbol "transfer", from, to, asset] and data is the amount as an i128.
// An event that does not have that shape is not a transfer this deployment
// understands, and becomes a record with no movements — its cursor advances, so
// an unfamiliar token cannot wedge the stream.
func transferRecord(event protocol.EventInfo) (Record, error) {
	closedAt, err := time.Parse(time.RFC3339, event.LedgerClosedAt)
	if err != nil {
		return Record{}, fmt.Errorf("ingestion: event %s has unreadable close time %q: %w",
			event.ID, event.LedgerClosedAt, err)
	}

	record := Record{
		Cursor:     event.ID,
		ID:         "soroban:event:" + event.ID,
		TxHash:     event.TransactionHash,
		OccurredAt: closedAt.UTC(),
	}

	topics, err := decodeTopics(event)
	if err != nil {
		return Record{}, err
	}
	if len(topics) != 4 {
		return record, nil
	}
	if topics[0].Type != xdr.ScValTypeScvSymbol || string(*topics[0].Sym) != "transfer" {
		return record, nil
	}

	from, ok := addressOf(topics[1])
	if !ok {
		return record, nil
	}
	to, ok := addressOf(topics[2])
	if !ok {
		return record, nil
	}
	if topics[3].Type != xdr.ScValTypeScvString {
		return record, nil
	}
	assetType, code, issuer, ok := parseAsset(string(*topics[3].Str))
	if !ok {
		return record, nil
	}

	var value xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(event.ValueXDR, &value); err != nil {
		return Record{}, fmt.Errorf("ingestion: event %s has an undecodable value: %w",
			event.ID, err)
	}
	amount, err := stroopsOf(value)
	if err != nil {
		return Record{}, fmt.Errorf("ingestion: event %s: %w", event.ID, err)
	}
	if amount.Sign() <= 0 {
		// A zero or negative transfer moves nothing. Recorded as a no-op rather
		// than an error: a contract is free to emit one and it is not this
		// deployment's business to object.
		return record, nil
	}

	record.Movements = []Movement{{
		From: from, To: to,
		AssetType: assetType, AssetCode: code, AssetIssuer: issuer,
		Amount: amount,
	}}
	return record, nil
}

func decodeTopics(event protocol.EventInfo) ([]xdr.ScVal, error) {
	out := make([]xdr.ScVal, 0, len(event.TopicXDR))
	for i, encoded := range event.TopicXDR {
		var topic xdr.ScVal
		if err := xdr.SafeUnmarshalBase64(encoded, &topic); err != nil {
			return nil, fmt.Errorf("ingestion: event %s topic %d is undecodable: %w",
				event.ID, i, err)
		}
		out = append(out, topic)
	}
	return out, nil
}

// addressOf renders a Soroban address as the strkey this deployment tracks.
func addressOf(v xdr.ScVal) (string, bool) {
	if v.Type != xdr.ScValTypeScvAddress || v.Address == nil {
		return "", false
	}
	switch v.Address.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		return v.Address.AccountId.Address(), true
	case xdr.ScAddressTypeScAddressTypeContract:
		encoded, err := strkey.Encode(strkey.VersionByteContract, v.Address.ContractId[:])
		if err != nil {
			return "", false
		}
		return encoded, true
	default:
		return "", false
	}
}

// parseAsset reads the SEP-11 asset string a token event carries.
func parseAsset(asset string) (assetType, code, issuer string, ok bool) {
	if asset == "native" {
		return "native", "", "", true
	}
	code, issuer, found := strings.Cut(asset, ":")
	if !found || code == "" || issuer == "" {
		return "", "", "", false
	}
	if len(code) <= 4 {
		return "credit_alphanum4", code, issuer, true
	}
	return "credit_alphanum12", code, issuer, true
}

// stroopsOf converts a token amount to the ledger's integer, exactly or not at
// all.
func stroopsOf(v xdr.ScVal) (money.Stroops, error) {
	var n *big.Int
	switch v.Type {
	case xdr.ScValTypeScvI128:
		n = big.NewInt(int64(v.I128.Hi))
		n.Lsh(n, 64)
		n.Add(n, new(big.Int).SetUint64(uint64(v.I128.Lo)))
	case xdr.ScValTypeScvU128:
		n = new(big.Int).SetUint64(uint64(v.U128.Hi))
		n.Lsh(n, 64)
		n.Or(n, new(big.Int).SetUint64(uint64(v.U128.Lo)))
	case xdr.ScValTypeScvI64:
		n = big.NewInt(int64(*v.I64))
	case xdr.ScValTypeScvU64:
		n = new(big.Int).SetUint64(uint64(*v.U64))
	default:
		return 0, fmt.Errorf("a transfer amount of type %s cannot be read", v.Type.String())
	}

	if !n.IsInt64() {
		// Refused rather than clamped. A clamped amount is a different payment
		// that every check downstream would agree with.
		return 0, fmt.Errorf("%w: %s", ErrAmountTooLarge, n.String())
	}
	return money.Stroops(n.Int64()), nil
}

// symbolVal and wildcard build the topic filter, which needs pointers.
func symbolVal(s string) *xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return &xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func wildcard() *string {
	star := "*"
	return &star
}
