package settlement

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// signedCall is a contract call as it comes back from a member's browser: with
// its footprint and resource fee, signed, and read out of XDR — which is the
// form the SDK's own fee-bump builder cannot wrap.
func signedCall(t *testing.T, c *Client, member *keypair.Full, resourceFee int64) *txnbuild.Transaction {
	t.Helper()
	var contract xdr.ContractId
	invoke := &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
				FunctionName:    "vote",
			},
		},
		Ext: xdr.TransactionExt{V: 1, SorobanData: &xdr.SorobanTransactionData{ResourceFee: xdr.Int64(resourceFee)}},
	}
	built, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: member.Address(), Sequence: 9},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimebounds(0, 1_700_000_000)},
		Operations:           []txnbuild.Operation{invoke},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	signed, err := built.Sign(c.Network(), member)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	wire, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	parsed, err := txnbuild.TransactionFromXDR(wire)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("not a transaction")
	}
	return tx
}

// A call somebody signed in a browser is wrapped for exactly what the network
// asks: its own resource fee, and an inclusion bid for it and for the wrapper.
//
// The SDK's builder cannot do this at all for a call read out of XDR, and the
// premise below is that it still cannot: the day it can, this function can go.
func TestAFeeBumpAroundASignedCallBidsWhatTheNetworkAsks(t *testing.T) {
	member, payer := keypair.MustRandom(), keypair.MustRandom()
	c := sorobanClient(t, nil)
	const resource = 482_113
	inner := signedCall(t, c, member, resource)

	if _, err := c.FeeBump(inner, payer.Address()); err == nil {
		t.Log("the SDK now wraps a parsed contract call; FeeBumpSoroban may no longer be needed")
	}

	bump, err := c.FeeBumpSoroban(inner, payer.Address())
	if err != nil {
		t.Fatalf("FeeBumpSoroban: %v", err)
	}
	// One base fee for the call and one for the wrapper, at this client's own
	// rate: the call was built bidding less, and the bump may not bid under it.
	if got, want := bump.MaxFee(), resource+2*c.baseFee; got != want {
		t.Errorf("bids %d stroops, want the resource fee and two base fees: %d", got, want)
	}
	if bump.FeeAccount() != payer.Address() {
		t.Errorf("paid by %s", bump.FeeAccount())
	}

	// What is inside is the member's transaction, to the byte, signature and
	// all.
	before, _ := inner.Base64()
	after, _ := bump.InnerTransaction().Base64()
	if before != after {
		t.Error("wrapping the call changed it")
	}
	signed, err := bump.Sign(c.Network(), payer)
	if err != nil || len(signed.Signatures()) != 1 {
		t.Fatalf("the payer could not sign it: %v", err)
	}

	// The payer is not the signer, so the ceiling is checked again here.
	greedy := signedCall(t, c, member, int64(MaxResourceFee)+1)
	if _, err := c.FeeBumpSoroban(greedy, payer.Address()); !errors.Is(err, ErrResourceFeeTooHigh) {
		t.Errorf("a call bidding past the ceiling: %v", err)
	}
}

// A call somebody else signed goes out inside the envelope that pays for it,
// and what is looked up afterwards is that envelope's hash, not theirs.
func TestAFeeBumpedCallIsSentWholeAndFoundByItsOuterHash(t *testing.T) {
	member := keypair.MustRandom()
	payer := keypair.MustRandom()
	c := sorobanClient(t, nil)

	inner := signedCall(t, c, member, 40_000)
	bump, err := c.FeeBumpSoroban(inner, payer.Address())
	if err != nil {
		t.Fatalf("FeeBumpSoroban: %v", err)
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
	if _, err := c.FeeBumpSoroban(plain, payer.Address()); !errors.Is(err, ErrNotSoroban) {
		t.Errorf("wrapping a payment: %v, want ErrNotSoroban", err)
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

// A rejection for a used sequence number is told apart from every other kind,
// whether it is the transaction's own or the one a fee bump carried: it is the
// one rejection no second attempt can put right.
func TestARejectionForAUsedSequenceNumberIsToldApart(t *testing.T) {
	result := func(outer xdr.TransactionResultCode, inner *xdr.TransactionResultCode) string {
		r := xdr.TransactionResult{Result: xdr.TransactionResultResult{Code: outer}}
		if inner != nil {
			r.Result.InnerResultPair = &xdr.InnerTransactionResultPair{
				Result: xdr.InnerTransactionResult{Result: xdr.InnerTransactionResultResult{Code: *inner}},
			}
		} else if outer == xdr.TransactionResultCodeTxFailed {
			r.Result.Results = &[]xdr.OperationResult{}
		}
		return encoded(t, r)
	}
	badSeq, badAuth := xdr.TransactionResultCodeTxBadSeq, xdr.TransactionResultCodeTxBadAuth

	cases := map[string]struct {
		xdr   string
		stale bool
		says  string
	}{
		"its own sequence":       {result(badSeq, nil), true, "TxBadSeq"},
		"inside a fee bump":      {result(xdr.TransactionResultCodeTxFeeBumpInnerFailed, &badSeq), true, "TxBadSeq"},
		"a bad signature":        {result(badAuth, nil), false, "TxBadAuth"},
		"a bad signature inside": {result(xdr.TransactionResultCodeTxFeeBumpInnerFailed, &badAuth), false, "TxBadAuth"},
		"nothing readable":       {"not xdr", false, "undecodable"},
	}
	for name, c := range cases {
		rpc := &fakeRPC{send: protocol.SendTransactionResponse{
			Status: stellarcore.TXStatusError, ErrorResultXDR: c.xdr,
		}}
		_, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if !errors.Is(err, ErrRejected) {
			t.Errorf("%s: %v, want a rejection", name, err)
			continue
		}
		if errors.Is(err, ErrBadSequence) != c.stale {
			t.Errorf("%s: stale = %v, want %v (%v)", name, !c.stale, c.stale, err)
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: the error does not carry %q: %v", name, c.says, err)
		}
	}
}
