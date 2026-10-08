package settlement

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func encoded(t *testing.T, v any) string {
	t.Helper()
	out, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func someContract() string {
	var id [32]byte
	for i := range id {
		id[i] = byte(i + 1)
	}
	address, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		panic(err)
	}
	return address
}

// A read is a simulation and nothing else: it returns what the function
// returned and sends nothing.
func TestReadAsksAndSendsNothing(t *testing.T) {
	answer := ScU32(7)
	value := encoded(t, answer)
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 100),
		MinResourceFee:     100,
		Results:            []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &value}},
	}}
	// No Horizon behind it at all: a read that loaded the account it reads as
	// would be a round trip for a sequence number nothing uses.
	c := testClient(&fakeHorizon{}).WithSoroban(rpc)

	got, err := c.Read(context.Background(), CallRequest{
		Source:   keypair.MustRandom().Address(),
		Contract: someContract(),
		Function: "proposal_count",
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != xdr.ScValTypeScvU32 || uint32(*got.U32) != 7 {
		t.Errorf("read %+v, want u32 7", got)
	}
	if len(rpc.sent) != 0 {
		t.Errorf("a read submitted %d transaction(s)", len(rpc.sent))
	}
}

func TestReadRefusesWhatIsNotACall(t *testing.T) {
	c := testClient(&fakeHorizon{}).WithSoroban(&fakeRPC{})
	source := keypair.MustRandom().Address()

	for name, req := range map[string]CallRequest{
		"no source":      {Contract: someContract(), Function: "policy"},
		"not a contract": {Source: source, Contract: source, Function: "policy"},
		"not a name":     {Source: source, Contract: someContract(), Function: "po licy"},
	} {
		if _, err := c.Read(context.Background(), req); err == nil {
			t.Errorf("%s: read anyway", name)
		}
	}

	if _, err := testClient(&fakeHorizon{}).Read(context.Background(), CallRequest{
		Source: source, Contract: someContract(), Function: "policy",
	}); !errors.Is(err, ErrNoSoroban) {
		t.Errorf("with no endpoint: %v, want ErrNoSoroban", err)
	}
}

// The contract's own code is the one part of a refusal a caller can act on.
func TestAContractsRefusalCarriesItsCode(t *testing.T) {
	refusal := fmt.Errorf("%w: %s", ErrSimulationFailed,
		"HostError: Error(Contract, #9)\n\nEvent log (newest first):\n   0: [Diagnostic Event] ...")
	if code, ok := ContractErrorCode(refusal); !ok || code != 9 {
		t.Errorf("code %d, %v; want 9", code, ok)
	}

	for name, err := range map[string]error{
		"nothing":            nil,
		"some other failure": errors.New("Error(Contract, #9)"),
		"the host's refusal": fmt.Errorf("%w: HostError: Error(Auth, InvalidAction)", ErrSimulationFailed),
	} {
		if code, ok := ContractErrorCode(err); ok {
			t.Errorf("%s: read code %d out of it", name, code)
		}
	}
}

// The contract of an asset is a function of the asset and the network. The
// expected values are the ones the network itself reports for testnet.
func TestAnAssetsContractIsDerivedNotLookedUp(t *testing.T) {
	c := testClient(&fakeHorizon{})

	native, err := c.AssetContract(txnbuild.NativeAsset{})
	if err != nil {
		t.Fatalf("native: %v", err)
	}
	if native != "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" {
		t.Errorf("native XLM on testnet is %s", native)
	}

	usdc, err := c.AssetContract(txnbuild.CreditAsset{
		Code: "USDC", Issuer: "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5",
	})
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	if usdc != "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA" {
		t.Errorf("Circle's testnet USDC is %s", usdc)
	}
	if !strkey.IsValidContractAddress(usdc) || usdc == native {
		t.Errorf("not a distinct contract address: %s", usdc)
	}
}

// What a call returned is read back from the transaction that made it.
func TestASubmittedCallReportsWhatItReturned(t *testing.T) {
	returned := ScU32(3)
	meta := encoded(t, xdr.TransactionMeta{
		V: 3,
		V3: &xdr.TransactionMetaV3{
			SorobanMeta: &xdr.SorobanTransactionMeta{ReturnValue: returned},
		},
	})
	landed := protocol.GetTransactionResponse{
		TransactionDetails: protocol.TransactionDetails{
			Status: protocol.TransactionStatusSuccess, Ledger: 99, ResultMetaXDR: meta,
		},
	}
	rpc := &fakeRPC{
		send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending},
		gets: []protocol.GetTransactionResponse{landed},
	}
	c := sorobanClient(t, rpc)

	res, err := c.SubmitSoroban(context.Background(), contractCall(t))
	if err != nil {
		t.Fatalf("SubmitSoroban: %v", err)
	}
	var got xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(res.ReturnValue, &got); err != nil {
		t.Fatalf("return value %q: %v", res.ReturnValue, err)
	}
	if got.Type != xdr.ScValTypeScvU32 || uint32(*got.U32) != 3 {
		t.Errorf("returned %+v, want u32 3", got)
	}

	// Metadata the RPC did not send, or sent unreadably, is not a failure of
	// the transaction: it landed.
	for name, raw := range map[string]string{"absent": "", "garbled": "not xdr"} {
		landed.ResultMetaXDR = raw
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending},
			gets: []protocol.GetTransactionResponse{landed},
		}
		res, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if err != nil || res.ReturnValue != "" || res.Ledger != 99 {
			t.Errorf("%s metadata: %+v, %v", name, res, err)
		}
	}
}

// A call somebody else signed goes out inside the envelope that pays for it,
// and what is looked up afterwards is that envelope's hash, not theirs.
func TestAFeeBumpedCallIsSentWholeAndFoundByItsOuterHash(t *testing.T) {
	member := keypair.MustRandom()
	payer := keypair.MustRandom()
	c := sorobanClient(t, nil)

	inner := contractCall(t)
	inner, err := inner.Sign(c.Network(), member)
	if err != nil {
		t.Fatalf("sign inner: %v", err)
	}
	bump, err := c.FeeBump(inner, payer.Address())
	if err != nil {
		t.Fatalf("FeeBump: %v", err)
	}
	bump, err = bump.Sign(c.Network(), payer)
	if err != nil {
		t.Fatalf("sign bump: %v", err)
	}

	rpc := &fakeRPC{
		send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending},
		gets: []protocol.GetTransactionResponse{{
			TransactionDetails: protocol.TransactionDetails{
				Status: protocol.TransactionStatusSuccess, Ledger: 12,
			},
		}},
	}
	res, err := c.WithSoroban(rpc).SubmitSorobanFeeBump(context.Background(), bump)
	if err != nil {
		t.Fatalf("SubmitSorobanFeeBump: %v", err)
	}

	outer, _ := bump.HashHex(c.Network())
	innerHash, _ := inner.HashHex(c.Network())
	if res.Hash != outer || res.Hash == innerHash {
		t.Errorf("reported %s; the fee bump is %s and what it carries is %s", res.Hash, outer, innerHash)
	}
	want, _ := bump.Base64()
	if len(rpc.sent) != 1 || rpc.sent[0] != want {
		t.Errorf("sent %v, want the fee bump itself", rpc.sent)
	}
}

// Only a contract call goes out this way. A classic payment has its own road,
// and one sent down this one would be waited for by rules that are not its.
func TestAFeeBumpAroundAnythingElseIsRefused(t *testing.T) {
	member := keypair.MustRandom()
	payer := keypair.MustRandom()
	c := sorobanClient(t, &fakeRPC{})

	plain, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: member.Address(), Sequence: 1},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(60)},
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: payer.Address(), Amount: "1", Asset: txnbuild.NativeAsset{},
		}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	bump, err := c.FeeBump(plain, payer.Address())
	if err != nil {
		t.Fatalf("FeeBump: %v", err)
	}
	if _, err := c.SubmitSorobanFeeBump(context.Background(), bump); !errors.Is(err, ErrNotSoroban) {
		t.Errorf("got %v, want ErrNotSoroban", err)
	}
}

// Creating a contract reports where it will be, from what simulation said, and
// carries the footprint a signature has to cover.
func TestPrepareCreateReportsTheNewAddress(t *testing.T) {
	where := someContract()
	address, err := ScAddress(where)
	if err != nil {
		t.Fatal(err)
	}
	value := encoded(t, address)
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 4_000),
		MinResourceFee:     4_000,
		Results:            []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &value}},
	}}
	c := sorobanClient(t, rpc)
	source := keypair.MustRandom().Address()

	prepared, err := c.PrepareCreate(context.Background(), CreateRequest{
		Source:      source,
		WasmHash:    "e8f0f9216a35849880d77dc8dbedacce02a25faa9763059f000d2bb35e232a3f",
		Salt:        [32]byte{1, 2, 3},
		Constructor: []xdr.ScVal{ScU32(1)},
	})
	if err != nil {
		t.Fatalf("PrepareCreate: %v", err)
	}
	if prepared.Address != where {
		t.Errorf("address %s, want %s", prepared.Address, where)
	}
	op, ok := prepared.Transaction.Operations()[0].(*txnbuild.InvokeHostFunction)
	if !ok || op.HostFunction.CreateContractV2 == nil {
		t.Fatalf("not a contract creation: %T", prepared.Transaction.Operations()[0])
	}
	if op.Ext.SorobanData == nil || int64(op.Ext.SorobanData.ResourceFee) != 4_000 {
		t.Errorf("the footprint was not written into the envelope: %+v", op.Ext)
	}
	if got := len(op.HostFunction.CreateContractV2.ConstructorArgs); got != 1 {
		t.Errorf("%d constructor argument(s), want 1", got)
	}

	for name, req := range map[string]CreateRequest{
		"a hash that is not one": {Source: source, WasmHash: "abcd"},
		"no source":              {WasmHash: "e8f0f9216a35849880d77dc8dbedacce02a25faa9763059f000d2bb35e232a3f"},
	} {
		if _, err := c.PrepareCreate(context.Background(), req); err == nil {
			t.Errorf("%s: prepared anyway", name)
		}
	}
}
