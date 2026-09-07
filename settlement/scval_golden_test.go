package settlement

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Constructors for the golden corpus's Soroban values.
//
// Verbose on purpose. The SDK's nativeToScVal-style helpers guess at a type
// from a Go value, and a corpus whose whole job is to pin exact types must not
// have one inferred for it.

// goldenContract is a fixed contract id, so the corpus is byte-stable.
const goldenContract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"

// goldenContractAddress is the corpus's fixed contract, as an ScAddress.
func goldenContractAddress(t *testing.T) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, goldenContract)
	if err != nil {
		t.Fatalf("decode contract id: %v", err)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
}

func goldenInvoke(
	t *testing.T, auth []xdr.SorobanAuthorizationEntry, args ...xdr.ScVal,
) *txnbuild.InvokeHostFunction {
	t.Helper()

	// The first argument names the function, by convention of this corpus's
	// construction: args[0] is the symbol and the rest are the call's own.
	name := xdr.ScSymbol("call")
	if len(args) > 0 && args[0].Type == xdr.ScValTypeScvSymbol {
		name = *args[0].Sym
		args = args[1:]
	}

	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: goldenContractAddress(t),
				FunctionName:    name,
				Args:            args,
			},
		},
		Auth: auth,
	}
}

// goldenRootInvocation is the invocation both corpus auth entries authorise.
// Every entry needs one: an unset function arm cannot be encoded.
func goldenRootInvocation(t *testing.T) xdr.SorobanAuthorizedInvocation {
	t.Helper()
	return xdr.SorobanAuthorizedInvocation{
		Function: xdr.SorobanAuthorizedFunction{
			Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
			ContractFn: &xdr.InvokeContractArgs{
				ContractAddress: goldenContractAddress(t),
				FunctionName:    "burn",
			},
		},
	}
}

// goldenAuthBySource is an entry the transaction's own source authorises.
func goldenAuthBySource(t *testing.T) xdr.SorobanAuthorizationEntry {
	t.Helper()
	return xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount,
		},
		RootInvocation: goldenRootInvocation(t),
	}
}

func goldenAuthFor(t *testing.T, address string) xdr.SorobanAuthorizationEntry {
	t.Helper()
	var account xdr.AccountId
	if err := account.SetAddress(address); err != nil {
		t.Fatalf("set address: %v", err)
	}
	return xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
			Address: &xdr.SorobanAddressCredentials{
				Address: xdr.ScAddress{
					Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &account,
				},
				Nonce:                     7,
				SignatureExpirationLedger: 1000,
				// An explicit void rather than the zero value: a zero ScVal has
				// no arm set and cannot be encoded at all.
				Signature: scVoid(),
			},
		},
		RootInvocation: goldenRootInvocation(t),
	}
}

func scBool(b bool) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b}
}

func scVoid() xdr.ScVal { return xdr.ScVal{Type: xdr.ScValTypeScvVoid} }

func scU32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

func scI32(n int32) xdr.ScVal {
	v := xdr.Int32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvI32, I32: &v}
}

func scU64(n uint64) xdr.ScVal {
	v := xdr.Uint64(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
}

func scSymbol(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

func scString(s string) xdr.ScVal {
	v := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
}

func scBytes(b []byte) xdr.ScVal {
	v := xdr.ScBytes(b)
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &v}
}

func scAddress(t *testing.T, address string) xdr.ScVal {
	t.Helper()
	var account xdr.AccountId
	if err := account.SetAddress(address); err != nil {
		t.Fatalf("set address: %v", err)
	}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &account}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func scI64(n int64) xdr.ScVal {
	v := xdr.Int64(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}
}

func scTimepoint(n uint64) xdr.ScVal {
	v := xdr.TimePoint(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvTimepoint, Timepoint: &v}
}

func scDuration(n uint64) xdr.ScVal {
	v := xdr.Duration(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvDuration, Duration: &v}
}

// wideParts splits a decimal into `count` 64-bit limbs, most significant first,
// as two's complement across the whole width.
//
// The values that matter here cannot be written as Go literals — 2^256-1 does
// not fit in anything — so they are written as decimal strings and split, which
// is also how the browser reads them back.
func wideParts(t *testing.T, decimal string, count int) []uint64 {
	t.Helper()
	n, ok := new(big.Int).SetString(decimal, 10)
	if !ok {
		t.Fatalf("not a decimal: %q", decimal)
	}
	n.Mod(n, new(big.Int).Lsh(big.NewInt(1), uint(64*count)))

	limbs := make([]uint64, count)
	mask := new(big.Int).SetUint64(^uint64(0))
	for i := count - 1; i >= 0; i-- {
		limbs[i] = new(big.Int).And(n, mask).Uint64()
		n.Rsh(n, 64)
	}
	return limbs
}

func scI128FromString(t *testing.T, decimal string) xdr.ScVal {
	t.Helper()
	p := wideParts(t, decimal, 2)
	parts := xdr.Int128Parts{Hi: xdr.Int64(int64(p[0])), Lo: xdr.Uint64(p[1])}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

func scU128FromString(t *testing.T, decimal string) xdr.ScVal {
	t.Helper()
	p := wideParts(t, decimal, 2)
	parts := xdr.UInt128Parts{Hi: xdr.Uint64(p[0]), Lo: xdr.Uint64(p[1])}
	return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &parts}
}

func scI256FromString(t *testing.T, decimal string) xdr.ScVal {
	t.Helper()
	p := wideParts(t, decimal, 4)
	parts := xdr.Int256Parts{
		HiHi: xdr.Int64(int64(p[0])), HiLo: xdr.Uint64(p[1]),
		LoHi: xdr.Uint64(p[2]), LoLo: xdr.Uint64(p[3]),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvI256, I256: &parts}
}

func scU256FromString(t *testing.T, decimal string) xdr.ScVal {
	t.Helper()
	p := wideParts(t, decimal, 4)
	parts := xdr.UInt256Parts{
		HiHi: xdr.Uint64(p[0]), HiLo: xdr.Uint64(p[1]),
		LoHi: xdr.Uint64(p[2]), LoLo: xdr.Uint64(p[3]),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &parts}
}

func scVec(items ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(items)
	p := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

func scMap(pairs ...xdr.ScVal) xdr.ScVal {
	entries := make(xdr.ScMap, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		entries = append(entries, xdr.ScMapEntry{Key: pairs[i], Val: pairs[i+1]})
	}
	p := &entries
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &p}
}
