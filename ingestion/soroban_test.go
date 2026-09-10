package ingestion

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

// fakeEvents answers GetEvents. No network anywhere in this file: the retention
// window and the amount limits are exactly the conditions a live endpoint will
// not reproduce on demand, so they have to be fabricated to be tested at all.
type fakeEvents struct {
	resp protocol.GetEventsResponse
	err  error

	requests []protocol.GetEventsRequest
}

func (f *fakeEvents) GetEvents(
	_ context.Context, req protocol.GetEventsRequest,
) (protocol.GetEventsResponse, error) {
	f.requests = append(f.requests, req)
	return f.resp, f.err
}

func encode(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	out, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func scSym(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func scStr(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	str := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
}

func scAccount(t *testing.T, address string) xdr.ScVal {
	t.Helper()
	var account xdr.AccountId
	if err := account.SetAddress(address); err != nil {
		t.Fatalf("set address: %v", err)
	}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &account}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func scContract(t *testing.T, address string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, address)
	if err != nil {
		t.Fatalf("decode contract: %v", err)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

// scI128 splits a decimal into the two halves the wire carries.
func scI128(t *testing.T, decimal string) xdr.ScVal {
	t.Helper()
	n, ok := new(big.Int).SetString(decimal, 10)
	if !ok {
		t.Fatalf("not a decimal: %q", decimal)
	}
	n.Mod(n, new(big.Int).Lsh(big.NewInt(1), 128))
	lo := new(big.Int).And(n, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(n, 64)
	parts := xdr.Int128Parts{Hi: xdr.Int64(int64(hi.Uint64())), Lo: xdr.Uint64(lo.Uint64())}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

// cursorAt builds an event id for a ledger, the way the RPC spells one.
func cursorAt(ledgerSeq uint32, event uint32) string {
	return protocol.Cursor{Ledger: ledgerSeq, Tx: 1, Op: 0, Event: event}.String()
}

// transferEvent builds a well-formed token transfer event.
func transferEvent(
	t *testing.T, id string, from, to xdr.ScVal, asset, amount string,
) protocol.EventInfo {
	t.Helper()
	return protocol.EventInfo{
		EventType:       protocol.EventTypeContract,
		ID:              id,
		Ledger:          100,
		LedgerClosedAt:  "2026-09-10T00:00:00Z",
		TransactionHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		TopicXDR: []string{
			encode(t, scSym(t, "transfer")),
			encode(t, from),
			encode(t, to),
			encode(t, scStr(t, asset)),
		},
		ValueXDR: encode(t, scI128(t, amount)),
	}
}

func TestTransferEventsBecomeMovements(t *testing.T) {
	from := keypair.MustRandom().Address()
	to := keypair.MustRandom().Address()

	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events: []protocol.EventInfo{
			transferEvent(t, cursorAt(100, 0), scAccount(t, from), scAccount(t, to),
				"USDC:"+testIssuer, "2500000000"),
		},
		OldestLedger: 50,
		LatestLedger: 200,
	}}

	src, err := NewTransferSource(api, "transfers", 50)
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	records, err := src.Fetch(context.Background(), "", 200)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("%d records", len(records))
	}

	got := records[0].Movements
	if len(got) != 1 {
		t.Fatalf("%d movements", len(got))
	}
	if got[0].From != from || got[0].To != to {
		t.Errorf("movement = %+v", got[0])
	}
	if got[0].Amount != money.MustParse("250") {
		t.Errorf("amount = %s", got[0].Amount)
	}
	if got[0].AssetCode != "USDC" || got[0].AssetIssuer != testIssuer {
		t.Errorf("asset = %+v", got[0])
	}
	// The idempotency key is the chain's own event id, so a replayed page
	// cannot post the same transfer twice.
	if records[0].ID != "soroban:event:"+cursorAt(100, 0) {
		t.Errorf("id = %q", records[0].ID)
	}
}

// TestAContractTreasuryIsTracked is the reason this source exists. A DAO whose
// money sits in a contract emits transfers between C-addresses and produces no
// Horizon payment operation at all.
func TestAContractTreasuryIsTracked(t *testing.T) {
	const contract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"
	to := keypair.MustRandom().Address()

	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events: []protocol.EventInfo{
			transferEvent(t, cursorAt(100, 0), scContract(t, contract), scAccount(t, to),
				"native", "10000000"),
		},
		OldestLedger: 50,
	}}
	src, _ := NewTransferSource(api, "transfers", 50)

	records, err := src.Fetch(context.Background(), "", 200)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	movement := records[0].Movements[0]
	if movement.From != contract {
		t.Fatalf("from = %q, want the contract", movement.From)
	}
	if movement.AssetType != "native" {
		t.Errorf("asset type = %q", movement.AssetType)
	}
}

// TestARetentionGapStopsIngestion is the decision this whole source is
// arranged around.
//
// The endpoint has forgotten the ledgers this source left off at. Skipping
// ahead to what it still holds would leave the ledger missing transfers with
// nothing recording that they are missing, and every balance downstream quietly
// wrong. Stopping is loud and recoverable.
func TestARetentionGapStopsIngestion(t *testing.T) {
	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events: []protocol.EventInfo{
			transferEvent(t, cursorAt(9000, 0),
				scAccount(t, keypair.MustRandom().Address()),
				scAccount(t, keypair.MustRandom().Address()), "native", "1"),
		},
		// We left off at ledger 100; its history now starts at 8000.
		OldestLedger: 8000,
		LatestLedger: 9000,
	}}
	src, _ := NewTransferSource(api, "transfers", 50)

	_, err := src.Fetch(context.Background(), cursorAt(100, 0), 200)
	if !errors.Is(err, ErrRetentionGap) {
		t.Fatalf("error = %v, want ErrRetentionGap", err)
	}
	// It has to say where the hole is, or nobody can backfill it.
	for _, want := range []string{"100", "8000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name ledger %s: %v", want, err)
		}
	}
}

// TestReadingContinuesInsideTheWindow: the ordinary case must not trip the
// check. A history starting at the cursor's own ledger has swallowed nothing.
func TestReadingContinuesInsideTheWindow(t *testing.T) {
	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events:       []protocol.EventInfo{},
		OldestLedger: 100,
		LatestLedger: 9000,
	}}
	src, _ := NewTransferSource(api, "transfers", 50)

	if _, err := src.Fetch(context.Background(), cursorAt(100, 0), 200); err != nil {
		t.Fatalf("a cursor at the oldest retained ledger was refused: %v", err)
	}
	if _, err := src.Fetch(context.Background(), cursorAt(500, 0), 200); err != nil {
		t.Fatalf("a cursor inside the window was refused: %v", err)
	}
}

// TestStartingWithNoCursorNeedsADeliberateChoice: adopting whatever the RPC
// still has would silently skip everything before it, which is the same failure
// the retention check exists to prevent — just at the beginning.
func TestStartingWithNoCursorNeedsADeliberateChoice(t *testing.T) {
	api := &fakeEvents{resp: protocol.GetEventsResponse{OldestLedger: 8000}}
	src, _ := NewTransferSource(api, "transfers", 0)

	_, err := src.Fetch(context.Background(), "", 200)
	if !errors.Is(err, ErrRetentionGap) {
		t.Fatalf("error = %v, want ErrRetentionGap", err)
	}
}

// TestAnAmountTooLargeIsRefused: token amounts are i128 and this ledger's are
// int64 stroops. Truncating would record a different, smaller payment that
// every check downstream would agree with.
func TestAnAmountTooLargeIsRefused(t *testing.T) {
	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events: []protocol.EventInfo{
			transferEvent(t, cursorAt(100, 0),
				scAccount(t, keypair.MustRandom().Address()),
				scAccount(t, keypair.MustRandom().Address()),
				"native", "170141183460469231731687303715884105727"),
		},
		OldestLedger: 50,
	}}
	src, _ := NewTransferSource(api, "transfers", 50)

	_, err := src.Fetch(context.Background(), "", 200)
	if !errors.Is(err, ErrAmountTooLarge) {
		t.Fatalf("error = %v, want ErrAmountTooLarge", err)
	}
}

// TestAnUnfamiliarEventCannotWedgeTheStream: an event this deployment does not
// understand becomes a record with no movements. Its cursor still advances, so
// one odd token cannot stop every other tenant's money being recorded.
func TestAnUnfamiliarEventCannotWedgeTheStream(t *testing.T) {
	good := keypair.MustRandom().Address()

	odd := transferEvent(t, cursorAt(100, 1),
		scAccount(t, good), scAccount(t, good), "native", "1")
	// Three topics instead of four: a different event that happens to be called
	// transfer.
	odd.TopicXDR = odd.TopicXDR[:3]

	notATransfer := transferEvent(t, cursorAt(100, 2),
		scAccount(t, good), scAccount(t, good), "native", "1")
	notATransfer.TopicXDR[0] = encode(t, scSym(t, "mint"))

	unreadableAsset := transferEvent(t, cursorAt(100, 3),
		scAccount(t, good), scAccount(t, good), "USDC", "1")

	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events:       []protocol.EventInfo{odd, notATransfer, unreadableAsset},
		OldestLedger: 50,
	}}
	src, _ := NewTransferSource(api, "transfers", 50)

	records, err := src.Fetch(context.Background(), "", 200)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("%d records, want all three consumed", len(records))
	}
	for i, r := range records {
		if len(r.Movements) != 0 {
			t.Errorf("record %d posted a movement: %+v", i, r.Movements)
		}
		if r.Cursor == "" {
			t.Errorf("record %d has no cursor, so the stream would not advance", i)
		}
	}
}

// TestTheFilterAsksTheRpcForTransfers: a busy network emits far more events
// than this deployment could page through, so the narrowing has to happen at
// the endpoint.
func TestTheFilterAsksTheRpcForTransfers(t *testing.T) {
	api := &fakeEvents{resp: protocol.GetEventsResponse{OldestLedger: 50}}
	src, _ := NewTransferSource(api, "transfers", 50)

	if _, err := src.Fetch(context.Background(), "", 200); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("%d requests", len(api.requests))
	}
	req := api.requests[0]
	if req.StartLedger != 50 {
		t.Errorf("start ledger = %d", req.StartLedger)
	}
	if len(req.Filters) != 1 || len(req.Filters[0].Topics) != 1 {
		t.Fatalf("filters = %+v", req.Filters)
	}
	segments := req.Filters[0].Topics[0]
	if len(segments) != 4 {
		t.Fatalf("%d topic segments, want the function name and three arguments", len(segments))
	}
	if segments[0].ScVal == nil || string(*segments[0].ScVal.Sym) != "transfer" {
		t.Errorf("first segment = %+v", segments[0])
	}
	for i, seg := range segments[1:] {
		if seg.Wildcard == nil || *seg.Wildcard != "*" {
			t.Errorf("segment %d is not a wildcard: %+v", i+1, seg)
		}
	}
}

// TestASorobanTransferPostsToTheLedger runs the record through the ingester, so
// the source and the applier are proved to agree.
func TestASorobanTransferPostsToTheLedger(t *testing.T) {
	ctx := context.Background()
	db := store.New(testPool)

	usdc, err := db.Ledger().EnsureAsset(ctx, "USDC", testIssuer)
	must(t, err, "ensure USDC")
	org := newOrg(t, db, "")
	treasury, err := db.EnsureAccount(ctx, org.ID, ledger.AccountTreasury, "", "treasury")
	must(t, err, "ensure treasury")
	_, err = db.EnsureAccount(ctx, org.ID, ledger.AccountExternal, "", "external")
	must(t, err, "ensure external")

	// A contract address, which tracked_addresses could not hold until the
	// migration that came with this source.
	const contract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"
	must(t, db.TrackAddress(ctx, org.ID, contract, treasury, store.RoleTreasury),
		"track the contract treasury")

	api := &fakeEvents{resp: protocol.GetEventsResponse{
		Events: []protocol.EventInfo{
			transferEvent(t, cursorAt(100, 0),
				scAccount(t, keypair.MustRandom().Address()), scContract(t, contract),
				"USDC:"+testIssuer, "2500000000"),
		},
		OldestLedger: 50,
	}}
	src, _ := NewTransferSource(api, t.Name(), 50)

	ing, err := New(db, testPool, Config{PageSize: 200})
	must(t, err, "new ingester")

	n, err := ing.Once(ctx, src)
	must(t, err, "ingest")
	if n != 1 {
		t.Fatalf("consumed %d", n)
	}

	balance, err := db.Balance(ctx, org.ID, treasury, usdc)
	must(t, err, "balance")
	if balance != money.MustParse("250") {
		t.Fatalf("the contract treasury's balance is %s, want 250", balance)
	}

	// Replaying the same page must not credit it twice.
	_, err = ing.Once(ctx, src)
	must(t, err, "ingest again")
	balance, err = db.Balance(ctx, org.ID, treasury, usdc)
	must(t, err, "balance again")
	if balance != money.MustParse("250") {
		t.Fatalf("a replay changed the balance to %s", balance)
	}
}
