package api

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"

	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// codeOnChain is a Soroban RPC that knows one thing: what code a contract runs.
// A test changes it to stand in for an upgrade.
type codeOnChain struct {
	wasm string
}

func (c *codeOnChain) SimulateTransaction(context.Context, protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error) {
	return protocol.SimulateTransactionResponse{}, errors.New("not used")
}

func (c *codeOnChain) SendTransaction(context.Context, protocol.SendTransactionRequest) (protocol.SendTransactionResponse, error) {
	return protocol.SendTransactionResponse{}, errors.New("not used")
}

func (c *codeOnChain) GetTransaction(context.Context, protocol.GetTransactionRequest) (protocol.GetTransactionResponse, error) {
	return protocol.GetTransactionResponse{}, errors.New("not used")
}

func (c *codeOnChain) GetLedgerEntries(context.Context, protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error) {
	raw, _ := hex.DecodeString(c.wasm)
	var hash xdr.Hash
	copy(hash[:], raw)
	data := xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &xdr.ContractId{}},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val: xdr.ScVal{
				Type: xdr.ScValTypeScvContractInstance,
				Instance: &xdr.ScContractInstance{Executable: xdr.ContractExecutable{
					Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &hash,
				}},
			},
		},
	}
	encoded, err := xdr.MarshalBase64(data)
	if err != nil {
		return protocol.GetLedgerEntriesResponse{}, err
	}
	return protocol.GetLedgerEntriesResponse{Entries: []protocol.LedgerEntryResult{{DataXDR: encoded}}}, nil
}

func someContract() string {
	raw := make([]byte, 32)
	copy(raw, []byte("a contract somebody else wrote!"))
	address, _ := strkey.Encode(strkey.VersionByteContract, raw)
	return address
}

// TestAContractIsCalledOnlyWhileItRunsWhatWasAllowed: the code behind an
// address can be replaced at that address. What a workspace allowed is the
// code, and a contract whose code has changed is refused before anything is
// built.
func TestAContractIsCalledOnlyWhileItRunsWhatWasAllowed(t *testing.T) {
	f := newProposalFixture(t, "contract pin")
	ctx := context.Background()
	chain := &codeOnChain{wasm: strings.Repeat("ab", 32)}
	f.svc.settle = f.svc.settle.WithSoroban(chain)
	contract := someContract()

	// Off until the operator turns it on.
	if _, err := f.svc.AllowContract(ctx, f.scope, f.member, contract, "vault"); !errors.Is(err, ErrContractCallsOff) {
		t.Fatalf("with contract calls off: %v", err)
	}
	f.svc.cfg.ContractCalls = true

	allowed, err := f.svc.AllowContract(ctx, f.scope, f.member, contract, "vault")
	if err != nil {
		t.Fatalf("AllowContract: %v", err)
	}
	if allowed.Executable != "wasm" || allowed.WasmHash != strings.Repeat("ab", 32) {
		t.Fatalf("pinned %+v", allowed)
	}
	if err := f.svc.checkPinned(ctx, allowed); err != nil {
		t.Fatalf("an unchanged contract was refused: %v", err)
	}

	// Upgraded, at the same address.
	chain.wasm = strings.Repeat("cd", 32)
	_, err = f.svc.ProposeInvoke(ctx, f.scope, f.treasuryRow.ID, "vault", "withdraw", "", f.member)
	if !errors.Is(err, ErrContractChanged) {
		t.Fatalf("a contract with replaced code was called: %v", err)
	}
	if !strings.Contains(err.Error(), strings.Repeat("ab", 32)) || !strings.Contains(err.Error(), strings.Repeat("cd", 32)) {
		t.Errorf("the refusal does not say what changed: %v", err)
	}

	// A label nobody allowed, and a contract nobody allowed, are refused alike.
	if _, err := f.svc.ProposeInvoke(ctx, f.scope, f.treasuryRow.ID, "other", "x", "", f.member); !errors.Is(err, store.ErrContractNotAllowed) {
		t.Fatalf("an unknown label: %v", err)
	}
	if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
		t.Fatal("a refused call opened a proposal")
	}

	// And it is gone from another workspace's point of view entirely.
	elsewhere := Scope{Org: orgFor(t, "/elsewhere").ID, OwnerRef: f.scope.OwnerRef}
	if _, err := f.store.AllowedContractByLabel(ctx, elsewhere.Org, "vault"); !errors.Is(err, store.ErrContractNotAllowed) {
		t.Fatalf("another workspace sees this allowlist: %v", err)
	}
}

// Every argument says its type. "250" is a different value as an i128 and as
// a u32, and a contract decoding the wrong one is a mistake nobody notices
// until it is on chain.
func TestContractArgumentsAreTypedNeverGuessed(t *testing.T) {
	someone := keypair.MustRandom().Address()
	args, err := ParseContractArgs(`address:` + someone + ` i128:-250 u128:340282366920938463463374607431768211455 u32:7 bool:true symbol:pay string:"two words" bytes:00ff`)
	if err != nil {
		t.Fatalf("ParseContractArgs: %v", err)
	}
	if len(args) != 8 {
		t.Fatalf("read %d arguments, want 8", len(args))
	}
	if args[0].Type != xdr.ScValTypeScvAddress || args[1].Type != xdr.ScValTypeScvI128 || args[6].Type != xdr.ScValTypeScvString {
		t.Fatalf("types: %v %v %v", args[0].Type, args[1].Type, args[6].Type)
	}
	if string(*args[6].Str) != "two words" {
		t.Errorf("quoted string = %q", *args[6].Str)
	}
	// -250 in two's complement over 128 bits.
	hi, lo := int64(args[1].I128.Hi), uint64(args[1].I128.Lo)
	got := new(big.Int).Lsh(big.NewInt(hi), 64)
	got.Add(got, new(big.Int).SetUint64(lo))
	if got.Cmp(big.NewInt(-250)) != 0 {
		t.Errorf("i128:-250 encoded as %s", got)
	}
	if args[2].U128.Hi != xdr.Uint64(^uint64(0)) || args[2].U128.Lo != xdr.Uint64(^uint64(0)) {
		t.Errorf("the largest u128 was not encoded whole: %+v", args[2].U128)
	}

	for _, bad := range []string{
		"250",      // no type
		"i128:2.5", // no decimal points to misplace
		"u128:-1",  // out of range, not wrapped
		"i128:170141183460469231731687303715884105728",
		"u32:4294967296",
		"bool:maybe",
		"address:mum",
		`string:"unclosed`,
		"float:1.5",
	} {
		if _, err := ParseContractArgs(bad); !errors.Is(err, ErrBadArgument) && err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

var _ = settlement.ErrNoSoroban
