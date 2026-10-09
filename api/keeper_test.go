package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/settlement"
)

// rented is a network on which every entry has a lifetime and extending one
// costs in proportion to how far.
type rented struct {
	// left is how many ledgers each entry has, by its encoded key.
	left map[string]uint32
	// rate is what one ledger more costs, for one entry, in stroops.
	rate int64

	extensions []uint32
	spent      int64
	pending    func()
}

func (r *rented) GetLedgerEntries(
	_ context.Context, req protocol.GetLedgerEntriesRequest,
) (protocol.GetLedgerEntriesResponse, error) {
	const latest = 5_000_000
	resp := protocol.GetLedgerEntriesResponse{LatestLedger: latest}
	for _, key := range req.Keys {
		if left, ok := r.left[key]; ok {
			until := latest + left
			resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, LiveUntilLedgerSeq: &until})
		}
	}
	return resp, nil
}

func (r *rented) SimulateTransaction(
	_ context.Context, req protocol.SimulateTransactionRequest,
) (protocol.SimulateTransactionResponse, error) {
	var envelope xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(req.Transaction, &envelope); err != nil {
		return protocol.SimulateTransactionResponse{}, err
	}
	op := envelope.V1.Tx.Operations[0].Body.ExtendFootprintTtlOp
	if op == nil {
		return protocol.SimulateTransactionResponse{}, errors.New("rented: only extensions are sent here")
	}
	reach := uint32(op.ExtendTo)
	keys := envelope.V1.Tx.Ext.SorobanData.Resources.Footprint.ReadOnly

	var fee int64
	apply := make(map[string]uint32, len(keys))
	for _, key := range keys {
		encoded, _ := xdr.MarshalBase64(key)
		if had := r.left[encoded]; reach > had {
			fee += int64(reach-had) * r.rate
			apply[encoded] = reach
		}
	}
	r.pending = func() {
		for key, to := range apply {
			r.left[key] = to
		}
		r.extensions = append(r.extensions, reach)
		r.spent += fee
	}

	data := *envelope.V1.Tx.Ext.SorobanData
	data.ResourceFee = xdr.Int64(fee)
	encoded, err := xdr.MarshalBase64(data)
	return protocol.SimulateTransactionResponse{TransactionDataXDR: encoded, MinResourceFee: fee}, err
}

func (r *rented) SendTransaction(
	context.Context, protocol.SendTransactionRequest,
) (protocol.SendTransactionResponse, error) {
	if r.pending != nil {
		r.pending()
		r.pending = nil
	}
	return protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending}, nil
}

func (r *rented) GetTransaction(
	context.Context, protocol.GetTransactionRequest,
) (protocol.GetTransactionResponse, error) {
	return protocol.GetTransactionResponse{TransactionDetails: protocol.TransactionDetails{
		Status: protocol.TransactionStatusSuccess, Ledger: 5_000_001,
	}}, nil
}

type keeperFixture struct {
	*proposalFixture
	net      *rented
	codeKey  string
	treasury string
	instance string
}

func newKeeperFixture(t *testing.T, name string) *keeperFixture {
	t.Helper()
	ctx := context.Background()
	f := newProposalFixture(t, name)

	// Other suites in this package leave treasuries behind in the same
	// database. This one is about counting them, so it starts from none.
	if _, err := testPool.Exec(ctx, `DELETE FROM org_treasuries WHERE kind = 'contract'`); err != nil {
		t.Fatalf("clear contract treasuries: %v", err)
	}

	net := &rented{left: map[string]uint32{}, rate: 20}
	relay := keypair.MustRandom()
	f.svc.settle = f.svc.settle.WithSoroban(net)
	f.svc.cfg.Treasuries = &ContractTreasuryConfig{
		WasmHash:     treasuryCode,
		RelayAddress: relay.Address(),
		SignRelay: func(tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
			return tx.Sign(network.TestNetworkPassphrase, relay)
		},
	}

	encode := func(key xdr.LedgerKey, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out, err := xdr.MarshalBase64(key)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	address := anotherContract()
	if _, err := f.store.AddContractTreasury(ctx, f.scope.Org, address, "ops", f.identity); err != nil {
		t.Fatalf("add treasury: %v", err)
	}
	return &keeperFixture{
		proposalFixture: f, net: net, treasury: address,
		codeKey:  encode(settlement.ContractCodeKey(treasuryCode)),
		instance: encode(settlement.ContractInstanceKey(address)),
	}
}

// With plenty of lifetime left there is nothing to do, and nothing is done:
// no transaction, no cost and nothing to report.
func TestNothingIsExtendedThatHasPlentyLeft(t *testing.T) {
	f := newKeeperFixture(t, "keeper idle")
	f.net.left[f.codeKey], f.net.left[f.instance] = keepFloor, keepFloor+1

	report, err := f.svc.KeepContractsAlive(context.Background())
	if err != nil || len(report) != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if len(f.net.extensions) != 0 || f.net.spent != 0 {
		t.Errorf("extended %v for %d stroops, with nothing running low", f.net.extensions, f.net.spent)
	}
}

// What is running low is topped up to the target, the code apart from the
// treasuries, and what it cost is said.
func TestWhatIsRunningLowIsToppedUp(t *testing.T) {
	f := newKeeperFixture(t, "keeper low")
	f.net.left[f.codeKey], f.net.left[f.instance] = keepFloor-1, 100

	report, err := f.svc.KeepContractsAlive(context.Background())
	if err != nil {
		t.Fatalf("KeepContractsAlive: %v", err)
	}
	if len(report) != 2 {
		t.Fatalf("report %+v", report)
	}
	code, treasuries := report[0], report[1]
	if code.What != "the treasury code" || code.Had != keepFloor-1 || code.Now != keepTarget || code.Problem != "" {
		t.Errorf("code: %+v", code)
	}
	if want := money.Stroops(int64(keepTarget-(keepFloor-1)) * f.net.rate); code.Cost != want {
		t.Errorf("code cost %s, want %s", code.Cost, want)
	}
	if treasuries.What != "1 treasury" || treasuries.Had != 100 || treasuries.Now != keepTarget {
		t.Errorf("treasuries: %+v", treasuries)
	}
	if f.net.left[f.codeKey] != keepTarget || f.net.left[f.instance] != keepTarget {
		t.Errorf("on the network: code %d, treasury %d", f.net.left[f.codeKey], f.net.left[f.instance])
	}

	// And now there is nothing to do.
	report, err = f.svc.KeepContractsAlive(context.Background())
	if err != nil || len(report) != 0 {
		t.Errorf("a second pass: %+v, %v", report, err)
	}
}

// When reaching the target would cost more than one extension may, less is
// reached for. The deployment pays what it decided to and gets what that buys.
func TestAnExtensionThatCostsTooMuchIsAskedForSmaller(t *testing.T) {
	f := newKeeperFixture(t, "keeper dear")
	f.net.left[f.codeKey], f.net.left[f.instance] = keepFloor-1, keepTarget
	// At this rate the whole reach costs about five times the ceiling.
	f.net.rate = int64(keepCeiling) * 5 / int64(keepTarget-keepFloor)

	report, err := f.svc.KeepContractsAlive(context.Background())
	if err != nil || len(report) != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	got := report[0]
	if got.Problem != "" || got.Now <= got.Had || got.Now >= keepTarget {
		t.Fatalf("reached %d from %d, with target %d: %+v", got.Now, got.Had, keepTarget, got)
	}
	if got.Cost > keepCeiling {
		t.Errorf("paid %s for one extension; the limit is %s", got.Cost, keepCeiling)
	}
	if len(f.net.extensions) != 1 {
		t.Errorf("sent %d extensions", len(f.net.extensions))
	}

	// And when even an hour is over the limit, nothing is sent and it says so.
	f.net.left[f.codeKey] = keepFloor - 1
	f.net.rate = int64(keepCeiling)
	report, err = f.svc.KeepContractsAlive(context.Background())
	if err != nil || len(report) != 1 || !strings.Contains(report[0].Problem, "even an hour more costs over the limit") {
		t.Fatalf("report %+v, %v", report, err)
	}
	if len(f.net.extensions) != 1 {
		t.Errorf("an extension over the limit was sent anyway")
	}
}

// Code that is not on the network is the one thing here that stops every
// treasury, and is said to be that, with what to do about it.
func TestMissingCodeIsSaidToBeMissing(t *testing.T) {
	f := newKeeperFixture(t, "keeper missing")
	// Neither the code nor the treasury is on this network at all.

	report, err := f.svc.KeepContractsAlive(context.Background())
	if err != nil || len(report) != 2 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if !strings.Contains(report[0].Problem, "stellar contract upload") {
		t.Errorf("code: %+v", report[0])
	}
	if report[1].What != "1 of 1 treasuries" || !strings.Contains(report[1].Problem, "archived") {
		t.Errorf("treasuries: %+v", report[1])
	}
	if len(f.net.extensions) != 0 {
		t.Error("something was extended that is not there")
	}

	// A deployment that is read-only, or not set up for this, spends nothing.
	f.net.left[f.codeKey] = 1
	f.svc.cfg.ReadOnly = true
	if report, err := f.svc.KeepContractsAlive(context.Background()); err != nil || report != nil {
		t.Errorf("read-only: %+v, %v", report, err)
	}
	f.svc.cfg.ReadOnly, f.svc.cfg.Treasuries = false, nil
	if report, err := f.svc.KeepContractsAlive(context.Background()); err != nil || report != nil {
		t.Errorf("unconfigured: %+v, %v", report, err)
	}
	if len(f.net.extensions) != 0 {
		t.Error("it spent anyway")
	}
}
