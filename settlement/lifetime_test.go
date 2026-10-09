package settlement

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// lifeRPC is an RPC that knows how long some entries have left.
type lifeRPC struct {
	*fakeRPC
	latest uint32
	// until is each entry's last live ledger, by its encoded key. An entry not
	// in it is one the RPC does not return.
	until map[string]uint32
	asked [][]string
}

func (r *lifeRPC) GetLedgerEntries(
	_ context.Context, req protocol.GetLedgerEntriesRequest,
) (protocol.GetLedgerEntriesResponse, error) {
	r.asked = append(r.asked, req.Keys)
	resp := protocol.GetLedgerEntriesResponse{LatestLedger: r.latest}
	// Backwards, because nothing promises the order asked.
	for i := len(req.Keys) - 1; i >= 0; i-- {
		if until, ok := r.until[req.Keys[i]]; ok {
			resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{
				KeyXDR: req.Keys[i], LiveUntilLedgerSeq: &until,
			})
		}
	}
	return resp, nil
}

func keyOf(t *testing.T, key xdr.LedgerKey) string {
	t.Helper()
	return encoded(t, key)
}

func TestLivesAreReportedForWhatWasAskedInTheOrderAsked(t *testing.T) {
	code, err := ContractCodeKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	here, err := ContractInstanceKey(someContract())
	if err != nil {
		t.Fatal(err)
	}
	gone, err := ContractInstanceKey(testContractAddress)
	if err != nil {
		t.Fatal(err)
	}

	rpc := &lifeRPC{fakeRPC: &fakeRPC{}, latest: 1_000, until: map[string]uint32{
		keyOf(t, code): 1_500,
		// Already past: it has nothing left, and that is not a negative number.
		keyOf(t, here): 900,
	}}
	c := testClient(&fakeHorizon{}).WithSoroban(rpc)

	lives, err := c.Lives(context.Background(), []xdr.LedgerKey{here, gone, code, here})
	if err != nil {
		t.Fatalf("Lives: %v", err)
	}
	want := []Life{{Found: true}, {}, {Found: true, Remaining: 500}, {Found: true}}
	for i := range want {
		if lives[i] != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, lives[i], want[i])
		}
	}
	// One key asked about twice is one question.
	if len(rpc.asked) != 1 || len(rpc.asked[0]) != 3 {
		t.Errorf("asked %v", rpc.asked)
	}

	if _, err := ContractCodeKey("not a hash"); err == nil {
		t.Error("made a code key out of something that is not a hash")
	}
	if _, err := ContractInstanceKey(keypair.MustRandom().Address()); err == nil {
		t.Error("made an instance key out of an account")
	}
	if _, err := testClient(&fakeHorizon{}).Lives(context.Background(), []xdr.LedgerKey{code}); !errors.Is(err, ErrNoSoroban) {
		t.Errorf("with no endpoint: %v", err)
	}
}

// More entries than the RPC answers for at once are asked about in pieces,
// and every one of them is still answered for.
func TestLivesAreReadInPiecesTheRPCWillAnswer(t *testing.T) {
	rpc := &lifeRPC{fakeRPC: &fakeRPC{}, latest: 10, until: map[string]uint32{}}
	var keys []xdr.LedgerKey
	for i := 0; i < maxKeysPerRead+7; i++ {
		var id xdr.ContractId
		id[0], id[1] = byte(i), byte(i>>8)
		key := xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		}
		keys = append(keys, key)
		rpc.until[keyOf(t, key)] = uint32(10 + i)
	}
	lives, err := testClient(&fakeHorizon{}).WithSoroban(rpc).Lives(context.Background(), keys)
	if err != nil {
		t.Fatalf("Lives: %v", err)
	}
	if len(rpc.asked) != 2 || len(rpc.asked[0]) != maxKeysPerRead || len(rpc.asked[1]) != 7 {
		t.Fatalf("asked in pieces of %d and %d", len(rpc.asked[0]), len(rpc.asked[len(rpc.asked)-1]))
	}
	for i, life := range lives {
		if !life.Found || life.Remaining != uint32(i) {
			t.Fatalf("entry %d: %+v", i, life)
		}
	}
}

// An extension is built for exactly the reach asked for, carries what the
// network said it costs, and is refused when that is more than the caller is
// willing to pay for one.
func TestAnExtensionIsSizedAndCapped(t *testing.T) {
	code, _ := ContractCodeKey(strings.Repeat("ab", 32))
	source := keypair.MustRandom().Address()
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 13_000_000), MinResourceFee: 13_000_000,
	}}
	c := sorobanClient(t, rpc)

	prepared, err := c.PrepareExtension(context.Background(), source, []xdr.LedgerKey{code}, 86_400, money.Stroops(20_000_000))
	if err != nil {
		t.Fatalf("PrepareExtension: %v", err)
	}
	if prepared.ResourceFee != 13_000_000 {
		t.Errorf("resource fee %s", prepared.ResourceFee)
	}
	op, ok := prepared.Transaction.Operations()[0].(*txnbuild.ExtendFootprintTtl)
	if !ok || op.ExtendTo != 86_400 {
		t.Fatalf("built %T reaching %d", prepared.Transaction.Operations()[0], op.ExtendTo)
	}
	if op.Ext.SorobanData == nil || int64(op.Ext.SorobanData.ResourceFee) != 13_000_000 {
		t.Errorf("the footprint was not written into the envelope")
	}
	if len(prepared.Transaction.Signatures()) != 0 {
		t.Error("it came back signed")
	}

	// The same extension, to somebody who will pay less for one.
	_, err = c.PrepareExtension(context.Background(), source, []xdr.LedgerKey{code}, 86_400, money.Stroops(10_000_000))
	if !errors.Is(err, ErrResourceFeeTooHigh) {
		t.Errorf("over the caller's ceiling: %v", err)
	}
	for name, try := range map[string]func() error{
		"nothing to extend": func() error {
			_, err := c.PrepareExtension(context.Background(), source, nil, 86_400, 1)
			return err
		},
		"no reach": func() error {
			_, err := c.PrepareExtension(context.Background(), source, []xdr.LedgerKey{code}, 0, 1)
			return err
		},
	} {
		if try() == nil {
			t.Errorf("%s: prepared anyway", name)
		}
	}
}
