package settlement

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// Calling a contract, from the outside, without pretending to understand it.
//
// stelfin does not know what a contract's functions mean and will not guess.
// What it can do is build the call exactly as asked, prove it would succeed,
// price it, and show it — which is the whole job.

// symbolPattern is what Soroban accepts as a symbol.
//
// Checked here rather than left to the network. A function name that fails at
// the host is a wasted round trip; a function name with a character the
// description escapes is a name somebody could make render as another.
var symbolPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,31}$`)

// CallRequest describes one contract invocation.
type CallRequest struct {
	// Source pays the fee and, unless an argument says otherwise, is who the
	// contract sees as the caller.
	Source string
	// Contract is the C… address being called.
	Contract string
	// Function is the exported name to invoke.
	Function string
	// Args are the arguments, already typed.
	//
	// Typed by the caller rather than inferred here. A helper that turned a Go
	// int into "whatever ScVal seems right" would pick u32 for a token amount
	// that has to be an i128, and the contract would reject it — or worse,
	// accept it as a different quantity.
	Args []xdr.ScVal
	// Auth is authorisation the caller has already collected.
	//
	// Left empty for a call the source account authorises itself; simulation
	// fills that in. Anything here is kept, because entries somebody signed are
	// not interchangeable with entries simulation recorded.
	Auth []xdr.SorobanAuthorizationEntry
	// Timeout bounds validity. Zero uses SorobanTimeout.
	Timeout time.Duration
}

// PreparedCall is a contract call ready to be signed.
type PreparedCall struct {
	// Transaction carries the footprint and the resource fee, so a signature
	// over it is a signature over what will actually be submitted.
	Transaction *txnbuild.Transaction
	// Simulation is what the network said the call would do and cost.
	Simulation *Simulation
}

// PrepareCall builds, simulates and assembles a contract call.
//
// The three steps are one function because doing two of them is a bug. A call
// that is built and signed without simulation carries no footprint and is
// rejected; a call that is simulated and signed without assembly carries a
// signature over the wrong bytes. Neither failure names itself.
func (c *Client) PrepareCall(ctx context.Context, req CallRequest) (*PreparedCall, error) {
	if c.soroban == nil {
		return nil, ErrNoSoroban
	}
	invoke, err := invocation(req)
	if err != nil {
		return nil, err
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = SorobanTimeout
	}

	built, err := c.Build(ctx, BuildRequest{
		Source:     req.Source,
		Operations: []txnbuild.Operation{invoke},
		Timeout:    timeout,
	})
	if err != nil {
		return nil, err
	}

	sim, err := c.Simulate(ctx, built)
	if err != nil {
		return nil, err
	}

	assembled, err := c.Assemble(built, sim)
	if err != nil {
		return nil, err
	}

	// Described last, and the result thrown away. The point is the refusal: a
	// call this deployment cannot render honestly must not reach a page that
	// has to show it, and finding that out here costs a round trip rather than
	// a confused signer.
	if _, err := c.DescribeTx(assembled); err != nil {
		return nil, err
	}

	return &PreparedCall{Transaction: assembled, Simulation: sim}, nil
}

// Argument constructors.
//
// Only the shapes stelfin itself needs. Everything else a caller builds
// directly, because a constructor that accepted anything would be a
// constructor that guessed at its type.

// ScAmount renders an exact stroop amount as the i128 a token contract expects.
//
// The one conversion worth having a function for. Token balances are i128, our
// amounts are int64 stroops, and the wrong integer width here is a payment for
// the wrong quantity that every layer downstream reports as correct.
func ScAmount(amount money.Stroops) xdr.ScVal {
	n := big.NewInt(int64(amount))
	parts := xdr.Int128Parts{
		Hi: xdr.Int64(new(big.Int).Rsh(n, 64).Int64()),
		Lo: xdr.Uint64(new(big.Int).And(n, new(big.Int).SetUint64(^uint64(0))).Uint64()),
	}
	// Rsh on a negative big.Int is an arithmetic shift, so Hi is already the
	// sign-extended high half and Lo the unsigned low one — the same two's
	// complement split scval.go reads back.
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

// ScAddress renders a G… or C… address as a Soroban address value.
func ScAddress(address string) (xdr.ScVal, error) {
	switch {
	case strkey.IsValidEd25519PublicKey(address):
		var account xdr.AccountId
		if err := account.SetAddress(address); err != nil {
			return xdr.ScVal{}, fmt.Errorf("settlement: %q: %w", address, err)
		}
		a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &account}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, nil

	case strkey.IsValidContractAddress(address):
		raw, err := strkey.Decode(strkey.VersionByteContract, address)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("settlement: %q: %w", address, err)
		}
		var id xdr.ContractId
		copy(id[:], raw)
		a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, nil

	default:
		return xdr.ScVal{}, fmt.Errorf("settlement: %q is neither an account nor a contract", address)
	}
}

// ScSymbol renders a Soroban symbol, refusing anything the host would.
func ScSymbol(s string) (xdr.ScVal, error) {
	if !symbolPattern.MatchString(s) {
		return xdr.ScVal{}, fmt.Errorf("settlement: %q is not a Soroban symbol", s)
	}
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}, nil
}

// ScU32 renders a u32, which is what a contract uses for a count or an id.
func ScU32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

// ScBool renders a boolean.
func ScBool(b bool) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b}
}

// Read calls a function for its answer and submits nothing.
//
// A simulation and no more: nothing is signed, nothing is sent and nothing is
// spent, so this is how the state of a contract is asked for. The answer is
// what the function would return against the ledger as it stands now — which
// is the only honest thing to show somebody about a contract, and the reason
// none of it is cached.
//
// The source is whoever is asking. It matters only to a function that reads
// its caller, and it is never loaded: a read has no sequence number to get
// right.
func (c *Client) Read(ctx context.Context, req CallRequest) (xdr.ScVal, error) {
	if c.soroban == nil {
		return xdr.ScVal{}, ErrNoSoroban
	}
	invoke, err := invocation(req)
	if err != nil {
		return xdr.ScVal{}, err
	}
	built, err := c.buildOn(
		&txnbuild.SimpleAccount{AccountID: req.Source},
		BuildRequest{Source: req.Source, Operations: []txnbuild.Operation{invoke}},
	)
	if err != nil {
		return xdr.ScVal{}, err
	}
	sim, err := c.Simulate(ctx, built)
	if err != nil {
		return xdr.ScVal{}, err
	}
	if sim.ReturnValue == "" {
		return xdr.ScVal{Type: xdr.ScValTypeScvVoid}, nil
	}
	var out xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(sim.ReturnValue, &out); err != nil {
		return xdr.ScVal{}, fmt.Errorf("settlement: decode what %s returned: %w", req.Function, err)
	}
	return out, nil
}

// invocation checks a call's parts and builds the operation that makes it.
func invocation(req CallRequest) (*txnbuild.InvokeHostFunction, error) {
	if !strkey.IsValidEd25519PublicKey(req.Source) {
		return nil, fmt.Errorf("settlement: %q is not an account", req.Source)
	}
	if !strkey.IsValidContractAddress(req.Contract) {
		return nil, fmt.Errorf("settlement: %q is not a contract address", req.Contract)
	}
	if !symbolPattern.MatchString(req.Function) {
		return nil, fmt.Errorf("settlement: %q is not a contract function name", req.Function)
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, req.Contract)
	if err != nil {
		return nil, fmt.Errorf("settlement: decode contract %s: %w", req.Contract, err)
	}
	var id xdr.ContractId
	copy(id[:], raw)

	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{
					Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id,
				},
				FunctionName: xdr.ScSymbol(req.Function),
				Args:         req.Args,
			},
		},
		Auth: req.Auth,
	}, nil
}

// contractErrorPattern finds a contract's own error code in what simulation
// said.
//
// The RPC reports a refusal as prose with the host's rendering of the error in
// it, and offers nothing more structured. The code is the one part of that
// text which is a contract's to define and a caller's to act on.
var contractErrorPattern = regexp.MustCompile(`Error\(Contract, #(\d+)\)`)

// ContractErrorCode reports the code a contract refused a simulated call with.
//
// False for anything else: a call that failed for a reason of the host's, or
// one that did not fail in simulation at all.
func ContractErrorCode(err error) (uint32, bool) {
	if err == nil || !errors.Is(err, ErrSimulationFailed) {
		return 0, false
	}
	found := contractErrorPattern.FindStringSubmatch(err.Error())
	if found == nil {
		return 0, false
	}
	code, convErr := strconv.ParseUint(found[1], 10, 32)
	if convErr != nil {
		return 0, false
	}
	return uint32(code), true
}

// AssetContract is the address of the contract every classic asset has: the
// one a Soroban contract holds and moves that asset through.
//
// Derived, not looked up. It is a function of the asset and the network, so
// two parties who agree on those agree on this without asking anybody — which
// is what lets a signing page check it.
func (c *Client) AssetContract(asset txnbuild.Asset) (string, error) {
	x, err := asset.ToXDR()
	if err != nil {
		return "", fmt.Errorf("settlement: asset: %w", err)
	}
	id, err := x.ContractID(c.network)
	if err != nil {
		return "", fmt.Errorf("settlement: contract of asset: %w", err)
	}
	return strkey.Encode(strkey.VersionByteContract, id[:])
}

// CreateRequest describes one contract to bring into existence.
type CreateRequest struct {
	// Source creates it and pays. The contract's address is derived from this
	// account and the salt, and the account gets nothing else for it.
	Source string
	// WasmHash is the hex hash of code already uploaded.
	WasmHash string
	// Salt distinguishes this contract from every other the same account has
	// created from the same code.
	Salt [32]byte
	// Constructor is what the contract is set up with, in the transaction that
	// creates it.
	Constructor []xdr.ScVal
}

// PreparedCreate is a contract creation ready to be signed, and where the
// contract will be.
type PreparedCreate struct {
	Transaction *txnbuild.Transaction
	// Address is what simulation said the new contract's address is.
	Address string
}

// PrepareCreate builds, simulates and assembles the creation of a contract.
//
// Not described and not shown to anybody, unlike a call: it is signed by the
// source, here, and never put in front of a member. What a member is entitled
// to check is what came of it — the code the new address runs and what it was
// set up with — and both are read back from the network afterwards.
func (c *Client) PrepareCreate(ctx context.Context, req CreateRequest) (*PreparedCreate, error) {
	if c.soroban == nil {
		return nil, ErrNoSoroban
	}
	if !strkey.IsValidEd25519PublicKey(req.Source) {
		return nil, fmt.Errorf("settlement: %q is not an account", req.Source)
	}
	code, err := hex.DecodeString(req.WasmHash)
	if err != nil || len(code) != 32 {
		return nil, fmt.Errorf("settlement: %q is not a code hash", req.WasmHash)
	}
	var wasm xdr.Hash
	copy(wasm[:], code)

	var deployer xdr.AccountId
	if err := deployer.SetAddress(req.Source); err != nil {
		return nil, fmt.Errorf("settlement: %q: %w", req.Source, err)
	}

	create := &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeCreateContractV2,
			CreateContractV2: &xdr.CreateContractArgsV2{
				ContractIdPreimage: xdr.ContractIdPreimage{
					Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
					FromAddress: &xdr.ContractIdPreimageFromAddress{
						Address: xdr.ScAddress{
							Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &deployer,
						},
						Salt: xdr.Uint256(req.Salt),
					},
				},
				Executable: xdr.ContractExecutable{
					Type:     xdr.ContractExecutableTypeContractExecutableWasm,
					WasmHash: &wasm,
				},
				ConstructorArgs: req.Constructor,
			},
		},
	}

	built, err := c.Build(ctx, BuildRequest{
		Source:     req.Source,
		Operations: []txnbuild.Operation{create},
		Timeout:    SorobanTimeout,
	})
	if err != nil {
		return nil, err
	}
	sim, err := c.Simulate(ctx, built)
	if err != nil {
		return nil, err
	}
	assembled, err := c.Assemble(built, sim)
	if err != nil {
		return nil, err
	}

	var returned xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(sim.ReturnValue, &returned); err != nil {
		return nil, fmt.Errorf("settlement: decode the new contract's address: %w", err)
	}
	if returned.Type != xdr.ScValTypeScvAddress || returned.Address == nil {
		return nil, errors.New("settlement: creating a contract did not return its address")
	}
	address, err := scAddressString(*returned.Address)
	if err != nil {
		return nil, err
	}
	return &PreparedCreate{Transaction: assembled, Address: address}, nil
}
