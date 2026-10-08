package treasuryabi

import (
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Back out of the contract's values.
//
// Every reader here is exact about the shape it accepts: the right type, every
// field present, nothing extra. What comes back from a contract is shown to
// people who will vote on it, and a reader that filled a gap with a zero would
// be showing them a proposal that does not exist.

// AsU32 reads a count or a proposal's number.
func AsU32(v xdr.ScVal) (uint32, error) {
	if v.Type != xdr.ScValTypeScvU32 || v.U32 == nil {
		return 0, fmt.Errorf("%w: wanted a u32, got %s", ErrShape, v.Type)
	}
	return uint32(*v.U32), nil
}

func asU64(v xdr.ScVal) (uint64, error) {
	if v.Type != xdr.ScValTypeScvU64 || v.U64 == nil {
		return 0, fmt.Errorf("%w: wanted a u64, got %s", ErrShape, v.Type)
	}
	return uint64(*v.U64), nil
}

// AsAmount reads an amount of a token, exactly.
func AsAmount(v xdr.ScVal) (*big.Int, error) {
	if v.Type != xdr.ScValTypeScvI128 || v.I128 == nil {
		return nil, fmt.Errorf("%w: wanted an i128, got %s", ErrShape, v.Type)
	}
	n := big.NewInt(int64(v.I128.Hi))
	n.Lsh(n, 64)
	// Added, not or-ed: the low half is unsigned and the value is two's
	// complement across all 128 bits.
	n.Add(n, new(big.Int).SetUint64(uint64(v.I128.Lo)))
	return n, nil
}

// AsAddress reads an address as the strkey a person recognises.
func AsAddress(v xdr.ScVal) (string, error) {
	if v.Type != xdr.ScValTypeScvAddress || v.Address == nil {
		return "", fmt.Errorf("%w: wanted an address, got %s", ErrShape, v.Type)
	}
	switch v.Address.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		if v.Address.AccountId == nil {
			return "", fmt.Errorf("%w: an account address with no account", ErrShape)
		}
		return v.Address.AccountId.Address(), nil
	case xdr.ScAddressTypeScAddressTypeContract:
		if v.Address.ContractId == nil {
			return "", fmt.Errorf("%w: a contract address with no contract", ErrShape)
		}
		return strkey.Encode(strkey.VersionByteContract, v.Address.ContractId[:])
	default:
		return "", fmt.Errorf("%w: an address of type %s", ErrShape, v.Address.Type)
	}
}

// AsAddresses reads a list of addresses.
func AsAddresses(v xdr.ScVal) ([]string, error) {
	items, err := asVec(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		address, err := AsAddress(item)
		if err != nil {
			return nil, err
		}
		out = append(out, address)
	}
	return out, nil
}

// AsVote reads how a member voted: for, against, or not yet.
func AsVote(v xdr.ScVal) (approve, voted bool, err error) {
	switch v.Type {
	case xdr.ScValTypeScvVoid:
		return false, false, nil
	case xdr.ScValTypeScvBool:
		if v.B == nil {
			return false, false, fmt.Errorf("%w: a vote with no value", ErrShape)
		}
		return *v.B, true, nil
	default:
		return false, false, fmt.Errorf("%w: wanted a vote, got %s", ErrShape, v.Type)
	}
}

func asVec(v xdr.ScVal) ([]xdr.ScVal, error) {
	if v.Type != xdr.ScValTypeScvVec {
		return nil, fmt.Errorf("%w: wanted a list, got %s", ErrShape, v.Type)
	}
	if v.Vec == nil || *v.Vec == nil {
		return nil, nil
	}
	return **v.Vec, nil
}

// fieldsOf reads a struct into its fields, requiring exactly the names given.
func fieldsOf(v xdr.ScVal, names ...string) (map[string]xdr.ScVal, error) {
	if v.Type != xdr.ScValTypeScvMap || v.Map == nil || *v.Map == nil {
		return nil, fmt.Errorf("%w: wanted a struct, got %s", ErrShape, v.Type)
	}
	entries := **v.Map
	if len(entries) != len(names) {
		return nil, fmt.Errorf("%w: a struct of %d fields, wanted %d", ErrShape, len(entries), len(names))
	}
	out := make(map[string]xdr.ScVal, len(entries))
	for _, entry := range entries {
		if entry.Key.Type != xdr.ScValTypeScvSymbol || entry.Key.Sym == nil {
			return nil, fmt.Errorf("%w: a struct field that is not named", ErrShape)
		}
		out[string(*entry.Key.Sym)] = entry.Val
	}
	for _, name := range names {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("%w: no field called %q", ErrShape, name)
		}
	}
	return out, nil
}

// variantOf reads an enum: its name, and what it carries if anything.
func variantOf(v xdr.ScVal) (name string, payload []xdr.ScVal, err error) {
	items, err := asVec(v)
	if err != nil {
		return "", nil, err
	}
	if len(items) == 0 || items[0].Type != xdr.ScValTypeScvSymbol || items[0].Sym == nil {
		return "", nil, fmt.Errorf("%w: an enum with no name", ErrShape)
	}
	return string(*items[0].Sym), items[1:], nil
}

// AsPolicy reads the policy.
func AsPolicy(v xdr.ScVal) (Policy, error) {
	f, err := fieldsOf(v,
		"quorum_bps", "approval_bps", "voting_period", "timelock", "spend_limit", "spend_window")
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if p.QuorumBps, err = AsU32(f["quorum_bps"]); err != nil {
		return Policy{}, err
	}
	if p.ApprovalBps, err = AsU32(f["approval_bps"]); err != nil {
		return Policy{}, err
	}
	if p.VotingPeriod, err = asU64(f["voting_period"]); err != nil {
		return Policy{}, err
	}
	if p.Timelock, err = asU64(f["timelock"]); err != nil {
		return Policy{}, err
	}
	if p.SpendLimit, err = AsAmount(f["spend_limit"]); err != nil {
		return Policy{}, err
	}
	if p.SpendWindow, err = asU64(f["spend_window"]); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func asMember(v xdr.ScVal) (Member, error) {
	f, err := fieldsOf(v, "address", "weight")
	if err != nil {
		return Member{}, err
	}
	var m Member
	if m.Address, err = AsAddress(f["address"]); err != nil {
		return Member{}, err
	}
	if m.Weight, err = AsU32(f["weight"]); err != nil {
		return Member{}, err
	}
	return m, nil
}

func asPayment(v xdr.ScVal) (Payment, error) {
	f, err := fieldsOf(v, "token", "to", "amount")
	if err != nil {
		return Payment{}, err
	}
	var p Payment
	if p.Token, err = AsAddress(f["token"]); err != nil {
		return Payment{}, err
	}
	if p.To, err = AsAddress(f["to"]); err != nil {
		return Payment{}, err
	}
	if p.Amount, err = AsAmount(f["amount"]); err != nil {
		return Payment{}, err
	}
	return p, nil
}

// AsAction reads what a proposal does.
func AsAction(v xdr.ScVal) (Action, error) {
	name, payload, err := variantOf(v)
	if err != nil {
		return Action{}, err
	}
	if len(payload) != 1 {
		return Action{}, fmt.Errorf("%w: %s carries %d values, wanted one", ErrShape, name, len(payload))
	}
	a := Action{Kind: name}
	switch name {
	case Pay:
		p, err := asPayment(payload[0])
		if err != nil {
			return Action{}, err
		}
		a.Payment = &p
	case SetPolicy:
		p, err := AsPolicy(payload[0])
		if err != nil {
			return Action{}, err
		}
		a.Policy = &p
	case SetMember:
		m, err := asMember(payload[0])
		if err != nil {
			return Action{}, err
		}
		a.Member = &m
	case AllowRecipient, DenyRecipient:
		if a.Recipient, err = AsAddress(payload[0]); err != nil {
			return Action{}, err
		}
	case Revoke:
		if a.Target, err = AsU32(payload[0]); err != nil {
			return Action{}, err
		}
	default:
		return Action{}, fmt.Errorf("%w: no action called %q", ErrShape, name)
	}
	return a, nil
}

// AsStatus reads a proposal's state.
func AsStatus(v xdr.ScVal) (string, error) {
	name, payload, err := variantOf(v)
	if err != nil {
		return "", err
	}
	if len(payload) != 0 {
		return "", fmt.Errorf("%w: a status carrying a value", ErrShape)
	}
	switch name {
	case Open, Passed, Executed, Rejected, Cancelled:
		return name, nil
	default:
		return "", fmt.Errorf("%w: no status called %q", ErrShape, name)
	}
}

// AsProposal reads a proposal.
func AsProposal(v xdr.ScVal) (Proposal, error) {
	f, err := fieldsOf(v,
		"id", "proposer", "action", "memo", "status", "closes_at", "executable_at", "yes", "no")
	if err != nil {
		return Proposal{}, err
	}
	var p Proposal
	if p.ID, err = AsU32(f["id"]); err != nil {
		return Proposal{}, err
	}
	if p.Proposer, err = AsAddress(f["proposer"]); err != nil {
		return Proposal{}, err
	}
	if p.Action, err = AsAction(f["action"]); err != nil {
		return Proposal{}, err
	}
	memo := f["memo"]
	if memo.Type != xdr.ScValTypeScvBytes || memo.Bytes == nil || len(*memo.Bytes) != len(p.Memo) {
		return Proposal{}, fmt.Errorf("%w: a memo that is not 32 bytes", ErrShape)
	}
	copy(p.Memo[:], *memo.Bytes)
	if p.Status, err = AsStatus(f["status"]); err != nil {
		return Proposal{}, err
	}
	if p.ClosesAt, err = asU64(f["closes_at"]); err != nil {
		return Proposal{}, err
	}
	if p.ExecutableAt, err = asU64(f["executable_at"]); err != nil {
		return Proposal{}, err
	}
	if p.Yes, err = AsU32(f["yes"]); err != nil {
		return Proposal{}, err
	}
	if p.No, err = AsU32(f["no"]); err != nil {
		return Proposal{}, err
	}
	return p, nil
}
