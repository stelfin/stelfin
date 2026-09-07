package settlement

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
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

	invoke := &txnbuild.InvokeHostFunction{
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
