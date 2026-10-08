// Package treasuryabi is the dao_treasury contract as Go sees it: its values,
// its functions and its refusals.
//
// A contract call is a function name and some typed values, and the network
// checks none of it against what the caller meant. A struct sent with its
// fields in the wrong order, or an amount sent as the wrong width of integer,
// is either refused by the host or — worse — accepted as something else. So
// the encoding lives in one place, and that place is checked byte for byte
// against a file the contract's own test suite writes: see abi_test.go.
//
// Nothing here talks to a network. It turns Go values into the contract's and
// back, and names the contract's errors.
package treasuryabi

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Version is the shape of the contract this package speaks. An instance that
// answers anything else to version() is not one these encodings are for.
const Version = 2

// ErrShape reports a value that is not the shape the contract returns.
//
// Always a refusal and never a best effort. A proposal read with one field
// missing is not most of a proposal; it is a description nobody should act on.
var ErrShape = errors.New("treasuryabi: not the shape this contract uses")

// Policy is the treasury's rules. See contracts/dao-treasury/src/types.rs.
type Policy struct {
	QuorumBps   uint32
	ApprovalBps uint32
	// VotingPeriod, Timelock and SpendWindow are in seconds.
	VotingPeriod uint64
	Timelock     uint64
	// SpendLimit is in the token's smallest unit.
	SpendLimit  *big.Int
	SpendWindow uint64
}

// Member is an address and the weight its vote carries.
type Member struct {
	Address string
	Weight  uint32
}

// Payment is one payment out of the treasury.
type Payment struct {
	Token  string
	To     string
	Amount *big.Int
}

// What an action is. The names are the contract's own.
const (
	Pay            = "Pay"
	SetPolicy      = "SetPolicy"
	SetMember      = "SetMember"
	AllowRecipient = "AllowRecipient"
	DenyRecipient  = "DenyRecipient"
	Revoke         = "Revoke"
)

// Action is the one thing a proposal does. Kind says which field is set.
type Action struct {
	Kind string

	Payment   *Payment // Pay
	Policy    *Policy  // SetPolicy
	Member    *Member  // SetMember
	Recipient string   // AllowRecipient, DenyRecipient
	Target    uint32   // Revoke
}

// A proposal's state, as the contract names it.
const (
	Open      = "Open"
	Passed    = "Passed"
	Executed  = "Executed"
	Rejected  = "Rejected"
	Cancelled = "Cancelled"
)

// Proposal is one thing a treasury has been asked to do.
type Proposal struct {
	ID       uint32
	Proposer string
	Action   Action
	Memo     [32]byte
	Status   string
	// ClosesAt and ExecutableAt are seconds since the epoch, as the ledger
	// counts them.
	ClosesAt     uint64
	ExecutableAt uint64
	Yes, No      uint32
}

// ---------------------------------------------------------------------------
// Into the contract's values
// ---------------------------------------------------------------------------

func symbol(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

func u32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

func u64(n uint64) xdr.ScVal {
	v := xdr.Uint64(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
}

var (
	minI128 = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
	maxI128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
	low64   = new(big.Int).SetUint64(^uint64(0))
)

// i128 renders an exact integer at the width the contract's amounts have.
func i128(n *big.Int) (xdr.ScVal, error) {
	if n == nil {
		return xdr.ScVal{}, fmt.Errorf("%w: an amount is missing", ErrShape)
	}
	if n.Cmp(minI128) < 0 || n.Cmp(maxI128) > 0 {
		return xdr.ScVal{}, fmt.Errorf("%w: %s does not fit an i128", ErrShape, n)
	}
	// An arithmetic shift, so the high half is already sign-extended and the
	// low half is the unsigned remainder: two's complement across the width.
	parts := xdr.Int128Parts{
		Hi: xdr.Int64(new(big.Int).Rsh(n, 64).Int64()),
		Lo: xdr.Uint64(new(big.Int).And(n, low64).Uint64()),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}, nil
}

// Address renders a G… or C… address.
func Address(address string) (xdr.ScVal, error) {
	switch {
	case strkey.IsValidEd25519PublicKey(address):
		var account xdr.AccountId
		if err := account.SetAddress(address); err != nil {
			return xdr.ScVal{}, fmt.Errorf("%w: %q: %v", ErrShape, address, err)
		}
		a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &account}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, nil
	case strkey.IsValidContractAddress(address):
		raw, err := strkey.Decode(strkey.VersionByteContract, address)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%w: %q: %v", ErrShape, address, err)
		}
		var id xdr.ContractId
		copy(id[:], raw)
		a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, nil
	default:
		return xdr.ScVal{}, fmt.Errorf("%w: %q is neither an account nor a contract", ErrShape, address)
	}
}

// U32 renders a proposal's number, or a count.
func U32(n uint32) xdr.ScVal { return u32(n) }

// Bool renders a vote.
func Bool(b bool) xdr.ScVal { return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b} }

// Amount renders an amount of a token, in its smallest unit.
func Amount(n *big.Int) (xdr.ScVal, error) { return i128(n) }

// Memo renders the commitment a proposal carries to what its members were
// shown.
func Memo(memo [32]byte) xdr.ScVal {
	b := xdr.ScBytes(memo[:])
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b}
}

func vec(items []xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(items)
	p := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

// structOf renders a struct: a map from field name to value, in the order the
// host requires, which is the names sorted. Sorted here rather than written
// out in order by hand, because a map out of order is refused by the host and
// the mistake is invisible at the call site.
func structOf(fields map[string]xdr.ScVal) xdr.ScVal {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make(xdr.ScMap, 0, len(names))
	for _, name := range names {
		entries = append(entries, xdr.ScMapEntry{Key: symbol(name), Val: fields[name]})
	}
	p := &entries
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &p}
}

// ScVal renders the policy.
func (p Policy) ScVal() (xdr.ScVal, error) {
	limit, err := i128(p.SpendLimit)
	if err != nil {
		return xdr.ScVal{}, err
	}
	return structOf(map[string]xdr.ScVal{
		"quorum_bps":    u32(p.QuorumBps),
		"approval_bps":  u32(p.ApprovalBps),
		"voting_period": u64(p.VotingPeriod),
		"timelock":      u64(p.Timelock),
		"spend_limit":   limit,
		"spend_window":  u64(p.SpendWindow),
	}), nil
}

// ScVal renders the member.
func (m Member) ScVal() (xdr.ScVal, error) {
	address, err := Address(m.Address)
	if err != nil {
		return xdr.ScVal{}, err
	}
	return structOf(map[string]xdr.ScVal{"address": address, "weight": u32(m.Weight)}), nil
}

// ScVal renders the payment.
func (p Payment) ScVal() (xdr.ScVal, error) {
	token, err := Address(p.Token)
	if err != nil {
		return xdr.ScVal{}, err
	}
	to, err := Address(p.To)
	if err != nil {
		return xdr.ScVal{}, err
	}
	amount, err := i128(p.Amount)
	if err != nil {
		return xdr.ScVal{}, err
	}
	return structOf(map[string]xdr.ScVal{"token": token, "to": to, "amount": amount}), nil
}

// ScVal renders the action: its name, then what it carries.
func (a Action) ScVal() (xdr.ScVal, error) {
	var (
		payload xdr.ScVal
		err     error
	)
	switch a.Kind {
	case Pay:
		if a.Payment == nil {
			return xdr.ScVal{}, fmt.Errorf("%w: a payment with nothing in it", ErrShape)
		}
		payload, err = a.Payment.ScVal()
	case SetPolicy:
		if a.Policy == nil {
			return xdr.ScVal{}, fmt.Errorf("%w: a policy change with no policy", ErrShape)
		}
		payload, err = a.Policy.ScVal()
	case SetMember:
		if a.Member == nil {
			return xdr.ScVal{}, fmt.Errorf("%w: a member change with no member", ErrShape)
		}
		payload, err = a.Member.ScVal()
	case AllowRecipient, DenyRecipient:
		payload, err = Address(a.Recipient)
	case Revoke:
		payload = u32(a.Target)
	default:
		return xdr.ScVal{}, fmt.Errorf("%w: no action called %q", ErrShape, a.Kind)
	}
	if err != nil {
		return xdr.ScVal{}, err
	}
	return vec([]xdr.ScVal{symbol(a.Kind), payload}), nil
}

// Members renders a list of members, for the constructor.
func Members(members []Member) (xdr.ScVal, error) {
	items := make([]xdr.ScVal, 0, len(members))
	for _, m := range members {
		v, err := m.ScVal()
		if err != nil {
			return xdr.ScVal{}, err
		}
		items = append(items, v)
	}
	return vec(items), nil
}

// Addresses renders a list of addresses, for the constructor's allowlist.
func Addresses(addresses []string) (xdr.ScVal, error) {
	items := make([]xdr.ScVal, 0, len(addresses))
	for _, a := range addresses {
		v, err := Address(a)
		if err != nil {
			return xdr.ScVal{}, err
		}
		items = append(items, v)
	}
	return vec(items), nil
}
