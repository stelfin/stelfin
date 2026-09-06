package settlement

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Rendering a Soroban value as text a person can read and a second
// implementation can reproduce exactly.
//
// Every value is tagged with its type, and that is not decoration. A contract
// argument of 1000 might be a token amount, a timestamp, an index or an account
// id, and the same digits mean different things in each case — so "1000" alone
// is not a description, it is a number next to a hope. "u64:1000" is a
// description.
//
// The tag also closes a collision. Without it, the string "5" and the integer 5
// render identically, and a contract that took either would let a caller show
// one thing and mean another. With it they are "str:5" and "u32:5".
//
// Anything this cannot render exactly is refused, exactly as the operation
// renderer refuses an operation it cannot show. A value summarised as
// "«object»" is a value nobody reviewed.

// scvalDelimiters are the characters that give the nested forms their shape.
//
// Escaped inside any value that could contain them, because otherwise a string
// argument of "a,b" and two arguments "a" and "b" would render to the same
// bytes — and two different transactions that produce the same description are
// exactly what the description exists to prevent.
const scvalDelimiters = ",[]{}="

// scvalString renders one Soroban value.
func scvalString(v xdr.ScVal) (string, error) {
	switch v.Type {
	case xdr.ScValTypeScvBool:
		return "bool:" + strconv.FormatBool(bool(*v.B)), nil

	case xdr.ScValTypeScvVoid:
		return "void:", nil

	case xdr.ScValTypeScvU32:
		return "u32:" + strconv.FormatUint(uint64(*v.U32), 10), nil
	case xdr.ScValTypeScvI32:
		return "i32:" + strconv.FormatInt(int64(*v.I32), 10), nil
	case xdr.ScValTypeScvU64:
		return "u64:" + strconv.FormatUint(uint64(*v.U64), 10), nil
	case xdr.ScValTypeScvI64:
		return "i64:" + strconv.FormatInt(int64(*v.I64), 10), nil

	case xdr.ScValTypeScvTimepoint:
		return "timepoint:" + strconv.FormatUint(uint64(*v.Timepoint), 10), nil
	case xdr.ScValTypeScvDuration:
		return "duration:" + strconv.FormatUint(uint64(*v.Duration), 10), nil

	// The wide integers are where a lossy renderer does real damage: a token
	// balance is an i128, and JavaScript's Number cannot hold one. Both sides
	// render the exact decimal — big.Int here, BigInt there — and never a
	// float.
	case xdr.ScValTypeScvU128:
		return "u128:" + u128String(*v.U128), nil
	case xdr.ScValTypeScvI128:
		return "i128:" + i128String(*v.I128), nil
	case xdr.ScValTypeScvU256:
		return "u256:" + u256String(*v.U256), nil
	case xdr.ScValTypeScvI256:
		return "i256:" + i256String(*v.I256), nil

	case xdr.ScValTypeScvSymbol:
		// Symbols are [a-zA-Z0-9_] by protocol rule, so nothing here can
		// collide with a delimiter. Escaped anyway: relying on a rule enforced
		// somewhere else is how the one value that breaks it gets through.
		return "sym:" + escapeScval(string(*v.Sym)), nil

	case xdr.ScValTypeScvString:
		return "str:" + escapeScval(string(*v.Str)), nil

	case xdr.ScValTypeScvBytes:
		return "bytes:" + hexOf(*v.Bytes), nil

	case xdr.ScValTypeScvAddress:
		address, err := scAddressString(*v.Address)
		if err != nil {
			return "", err
		}
		return "addr:" + address, nil

	case xdr.ScValTypeScvVec:
		if v.Vec == nil || *v.Vec == nil {
			return "vec:[]", nil
		}
		parts := make([]string, 0, len(**v.Vec))
		for i, item := range **v.Vec {
			rendered, err := scvalString(item)
			if err != nil {
				return "", fmt.Errorf("element %d: %w", i, err)
			}
			parts = append(parts, rendered)
		}
		return "vec:[" + strings.Join(parts, ",") + "]", nil

	case xdr.ScValTypeScvMap:
		if v.Map == nil || *v.Map == nil {
			return "map:{}", nil
		}
		// Written in the order the XDR carries. Soroban maps are sorted by key
		// at the protocol level, so this is deterministic without sorting here
		// — and sorting here would silently repair a map that was not, hiding
		// an envelope the network is going to reject anyway.
		parts := make([]string, 0, len(**v.Map))
		for i, entry := range **v.Map {
			key, err := scvalString(entry.Key)
			if err != nil {
				return "", fmt.Errorf("key %d: %w", i, err)
			}
			val, err := scvalString(entry.Val)
			if err != nil {
				return "", fmt.Errorf("value for key %d: %w", i, err)
			}
			parts = append(parts, key+"="+val)
		}
		return "map:{" + strings.Join(parts, ",") + "}", nil

	default:
		// Ledger keys, contract instances, nonce keys: values that appear in
		// footprints rather than in arguments. Refused rather than summarised,
		// because a value nobody can read is a value nobody reviewed.
		return "", fmt.Errorf("%w: a Soroban value of type %s cannot be shown",
			ErrIndescribable, v.Type.String())
	}
}

// scAddressString renders a Soroban address as the strkey a person recognises.
func scAddressString(a xdr.ScAddress) (string, error) {
	switch a.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		return a.AccountId.Address(), nil
	case xdr.ScAddressTypeScAddressTypeContract:
		return strkey.Encode(strkey.VersionByteContract, a.ContractId[:])
	default:
		return "", fmt.Errorf("%w: an address of type %s cannot be shown",
			ErrIndescribable, a.Type.String())
	}
}

// The wide integers, assembled exactly.
//
// A u128 arrives as two 64-bit halves and an i128 as a signed high half with an
// unsigned low one. Reassembling them with shifts on 64-bit integers overflows;
// with floats it rounds. big.Int does neither, and the browser's BigInt matches
// it.

func u128String(p xdr.UInt128Parts) string {
	n := new(big.Int).SetUint64(uint64(p.Hi))
	n.Lsh(n, 64)
	n.Or(n, new(big.Int).SetUint64(uint64(p.Lo)))
	return n.String()
}

func i128String(p xdr.Int128Parts) string {
	n := big.NewInt(int64(p.Hi))
	n.Lsh(n, 64)
	// Or would be wrong for a negative high half: the low half is unsigned and
	// the value is two's complement across the whole 128 bits, so it is added.
	n.Add(n, new(big.Int).SetUint64(uint64(p.Lo)))
	return n.String()
}

func u256String(p xdr.UInt256Parts) string {
	n := new(big.Int).SetUint64(uint64(p.HiHi))
	for _, part := range []uint64{uint64(p.HiLo), uint64(p.LoHi), uint64(p.LoLo)} {
		n.Lsh(n, 64)
		n.Or(n, new(big.Int).SetUint64(part))
	}
	return n.String()
}

func i256String(p xdr.Int256Parts) string {
	n := big.NewInt(int64(p.HiHi))
	for _, part := range []uint64{uint64(p.HiLo), uint64(p.LoHi), uint64(p.LoLo)} {
		n.Lsh(n, 64)
		n.Add(n, new(big.Int).SetUint64(part))
	}
	return n.String()
}

// escapeScval escapes the characters that would otherwise let two different
// values render to the same bytes.
//
// Backslash first, or an escaped delimiter would be re-escaped into something
// that decodes differently. Same rule and same order as canonical.go, extended
// with the characters that give the nested forms their shape.
func escapeScval(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString("\\\\")
		case r == '\n':
			b.WriteString("\\n")
		case r == '\r':
			b.WriteString("\\r")
		case r == '\t':
			b.WriteString("\\t")
		case r < 0x20 || strings.ContainsRune(scvalDelimiters, r):
			// Control characters go the same way as delimiters: a description
			// containing a raw escape sequence is a description that can be
			// made to render as something else in a terminal.
			b.WriteString(fmt.Sprintf("\\u%04x", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
