package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Calling somebody else's contract from a treasury.
//
// Three rules, and the order matters.
//
// It is off until the operator turns it on. A contract call can move money in
// ways no spend ceiling here can see — a ceiling bounds a payment, and a call
// is whatever the contract does — so a deployment that relies on its ceilings
// must not get contract calls by default.
//
// Only contracts the workspace has allowed, and only while they run the code
// they ran when they were allowed. An address is not a promise about
// behaviour: the code behind it can be replaced at the same address. The pin is
// checked before a call is built and again before it is submitted, so a
// contract upgraded while signatures were being collected is refused at the
// last moment rather than called.
//
// And it is a proposal like any other: built here, described from its own
// envelope, shown to the signers on the approval page, and executed only once
// the treasury's signers have signed it at the weight that spends.

var (
	// ErrContractCallsOff reports a deployment that has not turned contract
	// calls on.
	ErrContractCallsOff = errors.New("api: contract calls are not enabled on this deployment")

	// ErrContractChanged reports a contract running different code from the
	// code it was allowed with.
	ErrContractChanged = errors.New("api: the contract's code has changed since it was allowed")

	// ErrContractCallFailed reports a call that reached a ledger and failed
	// there.
	ErrContractCallFailed = errors.New("api: the contract call failed on chain")

	// ErrBadArgument reports a contract argument that cannot be read.
	ErrBadArgument = errors.New("api: that argument cannot be read")
)

func (s *Service) contractCallsOn() error {
	if !s.cfg.ContractCalls {
		return ErrContractCallsOff
	}
	if !s.settle.HasSoroban() {
		return settlement.ErrNoSoroban
	}
	return nil
}

// AllowContract adds a contract to a workspace's allowlist, pinned to what it
// runs now.
func (s *Service) AllowContract(
	ctx context.Context, scope Scope, by store.MemberID, address, label string,
) (store.AllowedContract, error) {
	if err := scope.check(); err != nil {
		return store.AllowedContract{}, err
	}
	if err := s.contractCallsOn(); err != nil {
		return store.AllowedContract{}, err
	}
	if !strkey.IsValidContractAddress(address) {
		return store.AllowedContract{}, fmt.Errorf("%w: %q is not a contract address", ErrBadArgument, address)
	}
	if !connectorName.MatchString(label) {
		return store.AllowedContract{}, ErrConnectorName
	}
	exe, err := s.settle.ContractExecutable(ctx, address)
	if err != nil {
		return store.AllowedContract{}, err
	}
	return s.store.AllowContract(ctx, store.AllowedContract{
		Org: scope.Org, Label: label, Address: address,
		Executable: exe.Kind, WasmHash: exe.WasmHash,
	}, by)
}

// checkPinned confirms a contract still runs what it was allowed with.
func (s *Service) checkPinned(ctx context.Context, allowed store.AllowedContract) error {
	exe, err := s.settle.ContractExecutable(ctx, allowed.Address)
	if err != nil {
		return err
	}
	if exe.Kind != allowed.Executable || exe.WasmHash != allowed.WasmHash {
		return fmt.Errorf("%w: %s was allowed running %s and now runs %s",
			ErrContractChanged, allowed.Label, pinned(allowed), exe)
	}
	return nil
}

func pinned(c store.AllowedContract) string {
	if c.Executable == "wasm" {
		return "wasm " + c.WasmHash
	}
	return c.Executable
}

// ProposeInvoke builds a call to an allowed contract from a treasury and opens
// it for approval.
func (s *Service) ProposeInvoke(
	ctx context.Context, scope Scope, treasuryID store.TreasuryID,
	label, function, arguments string, createdBy store.MemberID,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	if err := s.contractCallsOn(); err != nil {
		return nil, err
	}

	allowed, err := s.store.AllowedContractByLabel(ctx, scope.Org, label)
	if err != nil {
		return nil, err
	}
	if err := s.checkPinned(ctx, allowed); err != nil {
		return nil, err
	}
	args, err := ParseContractArgs(arguments)
	if err != nil {
		return nil, err
	}

	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, treasuryID)
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryClassic {
		return nil, fmt.Errorf("api: %s is a contract account; its authorisation is not signatures",
			treasury.Address)
	}

	call, err := s.settle.PrepareCall(ctx, settlement.CallRequest{
		Source: treasury.Address, Contract: allowed.Address, Function: function,
		Args: args, Timeout: org.ProposalTTL,
	})
	if err != nil {
		return nil, err
	}
	tx := call.Transaction

	description, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	canonical := description.Canonical()
	digest := sha256.Sum256([]byte(canonical))
	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash contract call: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode contract call: %w", err)
	}

	proposal, err := s.store.CreateProposal(ctx, store.CreateProposalParams{
		Org: scope.Org, Treasury: treasury.ID, Kind: "invoke",
		XDR: envelope, Hash: hash, SourceSeq: tx.SequenceNumber(),
		Description: canonical, Digest: digest[:],
		CreatedBy: createdBy,
		ExpiresAt: time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	})
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, scope, proposal, treasury, tx, description)
}

// invokedContract reads which contract a call envelope calls.
func invokedContract(tx *txnbuild.Transaction) (string, error) {
	ops := tx.Operations()
	if len(ops) != 1 {
		return "", errors.New("api: a contract call proposal holds exactly one operation")
	}
	invoke, ok := ops[0].(*txnbuild.InvokeHostFunction)
	if !ok || invoke.HostFunction.InvokeContract == nil {
		return "", errors.New("api: that proposal does not call a contract")
	}
	addr := invoke.HostFunction.InvokeContract.ContractAddress
	if addr.ContractId == nil {
		return "", errors.New("api: that call names no contract")
	}
	return strkey.Encode(strkey.VersionByteContract, addr.ContractId[:])
}

// ParseContractArgs reads a contract call's arguments, typed one by one.
//
// Each is type:value — address:G…, i128:250000000, u32:7, bool:true,
// symbol:transfer, string:"two words", bytes:00ff. Typed by the person and
// never guessed: "250" is a different value as an i128 and as a u32, and a
// contract decoding the wrong one is the kind of mistake nobody notices until
// it is on chain. Integers are whole numbers in the contract's own units, with
// no decimal point to misplace.
func ParseContractArgs(text string) ([]xdr.ScVal, error) {
	words, err := splitArguments(text)
	if err != nil {
		return nil, err
	}
	out := make([]xdr.ScVal, 0, len(words))
	for i, word := range words {
		kind, value, ok := strings.Cut(word, ":")
		if !ok {
			return nil, fmt.Errorf("%w: argument %d (%q) has no type, like i128:%s",
				ErrBadArgument, i+1, word, word)
		}
		v, err := scValOf(strings.ToLower(kind), value)
		if err != nil {
			return nil, fmt.Errorf("%w: argument %d: %v", ErrBadArgument, i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// splitArguments splits on spaces, keeping a "double-quoted" value whole.
func splitArguments(text string) ([]string, error) {
	var (
		out     []string
		current strings.Builder
		quoted  bool
		started bool
	)
	for _, r := range text {
		switch {
		case r == '"':
			quoted = !quoted
			started = true
		case (r == ' ' || r == '\t' || r == '\n') && !quoted:
			if started {
				out = append(out, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if quoted {
		return nil, fmt.Errorf("%w: a quote is not closed", ErrBadArgument)
	}
	if started {
		out = append(out, current.String())
	}
	return out, nil
}

func scValOf(kind, value string) (xdr.ScVal, error) {
	switch kind {
	case "address":
		return settlement.ScAddress(value)
	case "symbol":
		return settlement.ScSymbol(value)
	case "string":
		s := xdr.ScString(value)
		return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &s}, nil
	case "bool":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not true or false", value)
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b}, nil
	case "bytes":
		raw, err := hex.DecodeString(value)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not hex", value)
		}
		bytes := xdr.ScBytes(raw)
		return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &bytes}, nil
	case "u32":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not a u32", value)
		}
		u := xdr.Uint32(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}, nil
	case "i32":
		n, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not an i32", value)
		}
		i := xdr.Int32(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvI32, I32: &i}, nil
	case "u64":
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not a u64", value)
		}
		u := xdr.Uint64(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}, nil
	case "i64":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("%q is not an i64", value)
		}
		i := xdr.Int64(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &i}, nil
	case "i128", "u128":
		n, ok := new(big.Int).SetString(value, 10)
		if !ok {
			return xdr.ScVal{}, fmt.Errorf("%q is not a whole number", value)
		}
		return int128Of(kind, n)
	default:
		return xdr.ScVal{}, fmt.Errorf("%q is not a type this understands "+
			"(address, i128, u128, i64, u64, i32, u32, bool, symbol, string, bytes)", kind)
	}
}

// int128Of encodes a whole number as a 128-bit contract value, refusing one
// that does not fit rather than wrapping it.
func int128Of(kind string, n *big.Int) (xdr.ScVal, error) {
	one := big.NewInt(1)
	switch kind {
	case "u128":
		limit := new(big.Int).Lsh(one, 128)
		if n.Sign() < 0 || n.Cmp(limit) >= 0 {
			return xdr.ScVal{}, fmt.Errorf("%s does not fit in a u128", n)
		}
		hi := new(big.Int).Rsh(n, 64)
		lo := new(big.Int).And(n, new(big.Int).Sub(new(big.Int).Lsh(one, 64), one))
		parts := xdr.UInt128Parts{Hi: xdr.Uint64(hi.Uint64()), Lo: xdr.Uint64(lo.Uint64())}
		return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &parts}, nil
	default:
		limit := new(big.Int).Lsh(one, 127)
		if n.Cmp(limit) >= 0 || n.Cmp(new(big.Int).Neg(limit)) < 0 {
			return xdr.ScVal{}, fmt.Errorf("%s does not fit in an i128", n)
		}
		// Two's complement in 128 bits, then split.
		u := new(big.Int).Set(n)
		if u.Sign() < 0 {
			u.Add(u, new(big.Int).Lsh(one, 128))
		}
		hi := new(big.Int).Rsh(u, 64)
		lo := new(big.Int).And(u, new(big.Int).Sub(new(big.Int).Lsh(one, 64), one))
		parts := xdr.Int128Parts{Hi: xdr.Int64(int64(hi.Uint64())), Lo: xdr.Uint64(lo.Uint64())}
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}, nil
	}
}

// ProposeRestore puts to the signers the restore a contract call needs.
//
// The call is simulated again here rather than remembered, because what is
// archived is a fact about the ledger now, and the footprint to restore is
// whatever simulation says it is today.
func (s *Service) ProposeRestore(
	ctx context.Context, scope Scope, treasuryID store.TreasuryID,
	label, function, arguments string, createdBy store.MemberID,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	if err := s.contractCallsOn(); err != nil {
		return nil, err
	}
	allowed, err := s.store.AllowedContractByLabel(ctx, scope.Org, label)
	if err != nil {
		return nil, err
	}
	if err := s.checkPinned(ctx, allowed); err != nil {
		return nil, err
	}
	args, err := ParseContractArgs(arguments)
	if err != nil {
		return nil, err
	}
	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, treasuryID)
	if err != nil {
		return nil, err
	}

	_, err = s.settle.PrepareCall(ctx, settlement.CallRequest{
		Source: treasury.Address, Contract: allowed.Address, Function: function,
		Args: args, Timeout: org.ProposalTTL,
	})
	var archived *settlement.ArchivedStateError
	switch {
	case err == nil:
		return nil, ErrNothingArchived
	case !errors.As(err, &archived):
		return nil, err
	}

	tx, err := s.settle.BuildRestore(ctx, treasury.Address, archived, org.ProposalTTL)
	if err != nil {
		return nil, err
	}
	description, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	canonical := description.Canonical()
	digest := sha256.Sum256([]byte(canonical))
	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash restore: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode restore: %w", err)
	}
	proposal, err := s.store.CreateProposal(ctx, store.CreateProposalParams{
		Org: scope.Org, Treasury: treasury.ID, Kind: "restore",
		XDR: envelope, Hash: hash, SourceSeq: tx.SequenceNumber(),
		Description: canonical, Digest: digest[:], CreatedBy: createdBy,
		ExpiresAt: time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	})
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, scope, proposal, treasury, tx, description)
}

// ErrNothingArchived reports a restore asked for a call that needs none.
var ErrNothingArchived = errors.New("api: that call needs nothing restored")
