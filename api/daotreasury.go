package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/contracts/treasuryabi"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// A treasury held by a contract.
//
// The other kind of treasury here is a classic account, and the two are
// signed for in different ways. A classic account is one envelope that enough
// of its signers sign. A contract has no envelope to pass round: each member
// makes their own call to it — proposing, voting, withdrawing — from their
// own address, and the contract counts.
//
// So nothing in this file collects signatures, and nothing in it decides
// anything. It builds the one call a member asked for, proves by simulation
// that the contract would accept it, hands it to that member to sign, and
// sends it once they have. Whether a proposal has passed, who is a member and
// what may be paid are all read from the contract each time they are needed,
// because the contract is where they are true.
//
// The server holds one key in all of this and it is not a member's: it pays
// the fee on each member's call, and it sends the calls that need nobody's
// signature — creating a treasury, and carrying out a proposal the members
// have already passed. The contract gives that key no say in anything.

// memberCallLifetime is how long a member has to sign a call built for them.
//
// As long as a Soroban envelope reasonably stays valid. Longer, and the
// footprint simulation gave it has had time to go stale; shorter, and somebody
// reading what they are about to sign runs out of time doing it.
const memberCallLifetime = 10 * time.Minute

var (
	// ErrContractTreasuriesOff reports a deployment that cannot hold a
	// treasury in a contract: no Soroban RPC, or no treasury code uploaded to
	// the network it runs on.
	ErrContractTreasuriesOff = errors.New("api: contract treasuries are not available on this deployment")

	// ErrNotContractTreasury reports a treasury that is a classic account.
	ErrNotContractTreasury = errors.New("api: that treasury is not held by a contract")

	// ErrNotTreasuryCode reports a contract that is not running the treasury
	// code this deployment was built with.
	//
	// The whole of what a member is told about a contract treasury — that no
	// single key can move it — is a statement about particular code. A
	// contract at some address running anything else gets none of it.
	ErrNotTreasuryCode = errors.New("api: that contract is not running this deployment's treasury code")

	// ErrCallFailedOnChain reports a call that reached a ledger and was
	// refused there. It spent the signer's sequence number and nothing else.
	ErrCallFailedOnChain = errors.New("api: the contract refused the call on chain")

	// ErrNoWallet reports a member with no address to call from.
	ErrNoWallet = errors.New("api: this member has no wallet here")

	// ErrWalletNotOnChain reports a member whose address is not an account on
	// the network yet, and so cannot be the source of anything.
	ErrWalletNotOnChain = errors.New("api: that wallet is not on the network yet")
)

// ContractRefusal is the contract saying no in simulation, before anybody was
// asked to sign.
//
// The good way to be refused: it cost nothing, and the code says exactly why.
type ContractRefusal struct {
	Code treasuryabi.Code
}

func (e *ContractRefusal) Error() string {
	return fmt.Sprintf("api: the treasury contract refused (code %d)", e.Code)
}

// TokenRefusal is a token's contract saying no in simulation, to a transfer a
// member asked to make from their own wallet.
//
// Kept apart from a treasury's refusal because the numbers are not the same
// numbers. A token defines its own codes, and one of them read against the
// treasury's list would name a reason that had nothing to do with it.
type TokenRefusal struct {
	Code uint32
}

func (e *TokenRefusal) Error() string {
	return fmt.Sprintf("api: the token contract refused (code %d)", e.Code)
}

// ContractTreasuryConfig is what holding treasuries in a contract needs.
type ContractTreasuryConfig struct {
	// WasmHash is the code every treasury created here runs, and the only code
	// a contract adopted here may be running.
	WasmHash string
	// RelayAddress sends the calls that need nobody's signature, and pays for
	// them. It is an ordinary account holding fee money; the contract gives it
	// no authority.
	RelayAddress string
	// SignRelay signs as that account.
	SignRelay func(*txnbuild.Transaction) (*txnbuild.Transaction, error)
}

// relaying serialises what the relay account sends.
//
// One account has one sequence number, and two transactions built from it at
// once are built for the same one: the second to arrive is refused. Process
// wide, which is enough for a deployment that is one process, and merely a
// retry for one that is not.
var relaying sync.Mutex

func (s *Service) contractTreasuriesOn() (*ContractTreasuryConfig, error) {
	cfg := s.cfg.Treasuries
	if cfg == nil || cfg.WasmHash == "" || cfg.RelayAddress == "" || cfg.SignRelay == nil {
		return nil, ErrContractTreasuriesOff
	}
	if !s.settle.HasSoroban() {
		return nil, ErrContractTreasuriesOff
	}
	return cfg, nil
}

// refusalOf turns a simulation the contract refused into its reason.
func refusalOf(err error) error {
	if code, ok := settlement.ContractErrorCode(err); ok {
		return &ContractRefusal{Code: treasuryabi.Code(code)}
	}
	return err
}

// readTreasury asks a treasury contract a question.
func (s *Service) readTreasury(
	ctx context.Context, cfg *ContractTreasuryConfig, contract, function string, args ...xdr.ScVal,
) (xdr.ScVal, error) {
	v, err := s.settle.Read(ctx, settlement.CallRequest{
		Source: cfg.RelayAddress, Contract: contract, Function: function, Args: args,
	})
	if err != nil {
		return xdr.ScVal{}, refusalOf(err)
	}
	return v, nil
}

// TreasuryState is a contract treasury as the contract reports it now.
type TreasuryState struct {
	Treasury store.Treasury
	Policy   treasuryabi.Policy
	// Members carry the weight each was read with.
	Members     []treasuryabi.Member
	TotalWeight uint32
	Recipients  []string
	// Proposals is how many there have been. They are numbered from one.
	Proposals uint32
	// Spendable is what is left of the rolling allowance a member may spend
	// alone.
	Spendable *big.Int
}

// each runs every task and returns the first failure. The reads behind one
// answer are independent, and a treasury of five members is otherwise eight
// round trips made one after another.
func each(tasks ...func() error) error {
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = task()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ContractTreasuryState reads a contract treasury's rules, members and
// recipients from the contract.
func (s *Service) ContractTreasuryState(
	ctx context.Context, scope Scope, treasury store.Treasury,
) (*TreasuryState, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryContract || treasury.Org != scope.Org {
		return nil, ErrNotContractTreasury
	}
	return s.treasuryState(ctx, cfg, treasury)
}

func (s *Service) treasuryState(
	ctx context.Context, cfg *ContractTreasuryConfig, treasury store.Treasury,
) (*TreasuryState, error) {
	state := &TreasuryState{Treasury: treasury}
	var addresses []string

	err := each(
		func() error {
			v, err := s.readTreasury(ctx, cfg, treasury.Address, "policy")
			if err != nil {
				return err
			}
			state.Policy, err = treasuryabi.AsPolicy(v)
			return err
		},
		func() error {
			v, err := s.readTreasury(ctx, cfg, treasury.Address, "members")
			if err != nil {
				return err
			}
			addresses, err = treasuryabi.AsAddresses(v)
			return err
		},
		func() error {
			v, err := s.readTreasury(ctx, cfg, treasury.Address, "allowlist")
			if err != nil {
				return err
			}
			state.Recipients, err = treasuryabi.AsAddresses(v)
			return err
		},
		func() error {
			v, err := s.readTreasury(ctx, cfg, treasury.Address, "proposal_count")
			if err != nil {
				return err
			}
			state.Proposals, err = treasuryabi.AsU32(v)
			return err
		},
		func() error {
			v, err := s.readTreasury(ctx, cfg, treasury.Address, "spendable")
			if err != nil {
				return err
			}
			state.Spendable, err = treasuryabi.AsAmount(v)
			return err
		},
	)
	if err != nil {
		return nil, err
	}

	state.Members = make([]treasuryabi.Member, len(addresses))
	weights := make([]func() error, len(addresses))
	for i, address := range addresses {
		weights[i] = func() error {
			weight, err := s.memberWeight(ctx, cfg, treasury.Address, address)
			state.Members[i] = treasuryabi.Member{Address: address, Weight: weight}
			return err
		}
	}
	if err := each(weights...); err != nil {
		return nil, err
	}
	for _, m := range state.Members {
		state.TotalWeight += m.Weight
	}
	return state, nil
}

func (s *Service) memberWeight(
	ctx context.Context, cfg *ContractTreasuryConfig, contract, address string,
) (uint32, error) {
	who, err := treasuryabi.Address(address)
	if err != nil {
		return 0, err
	}
	v, err := s.readTreasury(ctx, cfg, contract, "member_weight", who)
	if err != nil {
		return 0, err
	}
	return treasuryabi.AsU32(v)
}

// noteDomain separates a proposal note's commitment from any other hash of
// the same text.
const noteDomain = "stelfin proposal note v1\n"

// noteMemo is the commitment a proposal carries to what its proposer wrote.
func noteMemo(note string) [32]byte {
	return sha256.Sum256([]byte(noteDomain + note))
}

// ChainProposal is a proposal as the contract holds it, with what this
// deployment can add.
type ChainProposal struct {
	treasuryabi.Proposal
	// Note is what the proposer wrote, when this deployment holds it and it
	// hashes to the commitment on chain. Otherwise empty: prose that does not
	// match its commitment is prose about some other proposal.
	Note string
	// Settles is what settling the proposal now would record, for one still
	// open: Passed, Rejected, or empty when the votes not yet cast could still
	// change it. Read by simulation, so it is the contract's own answer.
	Settles string
}

// ChainProposals lists a contract treasury's most recent proposals, newest
// first.
func (s *Service) ChainProposals(
	ctx context.Context, scope Scope, treasury store.Treasury, limit int,
) ([]ChainProposal, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryContract || treasury.Org != scope.Org {
		return nil, ErrNotContractTreasury
	}

	v, err := s.readTreasury(ctx, cfg, treasury.Address, "proposal_count")
	if err != nil {
		return nil, err
	}
	count, err := treasuryabi.AsU32(v)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || uint32(limit) > count {
		limit = int(count)
	}

	out := make([]ChainProposal, limit)
	reads := make([]func() error, limit)
	for i := range limit {
		id := count - uint32(i)
		reads[i] = func() error {
			p, err := s.chainProposal(ctx, cfg, treasury, id)
			if err != nil {
				return err
			}
			out[i] = *p
			return nil
		}
	}
	if err := each(reads...); err != nil {
		return nil, err
	}
	return out, s.attachNotes(ctx, scope, treasury, out)
}

// ChainProposalByID reads one proposal from a contract treasury.
func (s *Service) ChainProposalByID(
	ctx context.Context, scope Scope, treasury store.Treasury, id uint32,
) (*ChainProposal, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryContract || treasury.Org != scope.Org {
		return nil, ErrNotContractTreasury
	}
	p, err := s.chainProposal(ctx, cfg, treasury, id)
	if err != nil {
		return nil, err
	}
	one := []ChainProposal{*p}
	if err := s.attachNotes(ctx, scope, treasury, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

func (s *Service) chainProposal(
	ctx context.Context, cfg *ContractTreasuryConfig, treasury store.Treasury, id uint32,
) (*ChainProposal, error) {
	v, err := s.readTreasury(ctx, cfg, treasury.Address, "proposal", treasuryabi.U32(id))
	if err != nil {
		return nil, err
	}
	p, err := treasuryabi.AsProposal(v)
	if err != nil {
		return nil, err
	}
	out := &ChainProposal{Proposal: p}
	if p.Status != treasuryabi.Open {
		return out, nil
	}

	// What would settling it record? Asked of the contract, which is the only
	// place the arithmetic of quorum and approval is done.
	settled, err := s.readTreasury(ctx, cfg, treasury.Address, "tally", treasuryabi.U32(id))
	var refusal *ContractRefusal
	switch {
	case errors.As(err, &refusal) && refusal.Code == treasuryabi.Undecided:
		return out, nil
	case err != nil:
		return nil, err
	}
	out.Settles, err = treasuryabi.AsStatus(settled)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) attachNotes(
	ctx context.Context, scope Scope, treasury store.Treasury, proposals []ChainProposal,
) error {
	notes, err := s.store.ProposalNotes(ctx, scope.Org, treasury.ID)
	if err != nil {
		return err
	}
	for i := range proposals {
		held, ok := notes[proposals[i].ID]
		if !ok || held.Note == "" {
			continue
		}
		if noteMemo(held.Note) == proposals[i].Memo {
			proposals[i].Note = held.Note
		}
	}
	return nil
}

// TokenName names a token contract when it is one this deployment can
// account for: the network's own asset, or the one it is configured with.
// Anything else is named by its address, which is all that is known about it.
func (s *Service) TokenName(contract string) string {
	for _, asset := range []txnbuild.Asset{txnbuild.NativeAsset{}, s.cfg.Asset} {
		if asset == nil {
			continue
		}
		if address, err := s.settle.AssetContract(asset); err == nil && address == contract {
			return s.AssetName(asset)
		}
	}
	return "token " + contract
}

// TokenAmount writes an amount of a token for a person. Classic assets, which
// are every token named here, count in ten-millionths.
func TokenAmount(n *big.Int) string {
	if n == nil {
		return "0"
	}
	if n.IsInt64() {
		return money.Stroops(n.Int64()).Display()
	}
	return n.String() + " units"
}

// DescribeAction says in words what a proposal does.
//
// Written from the action as the contract holds it, never from what was asked
// for. The two should agree, and saying the second would hide it on the day
// they do not.
func (s *Service) DescribeAction(a treasuryabi.Action) string {
	switch a.Kind {
	case treasuryabi.Pay:
		return fmt.Sprintf("pay %s %s to %s",
			TokenAmount(a.Payment.Amount), s.TokenName(a.Payment.Token), a.Payment.To)
	case treasuryabi.SetPolicy:
		return "change the rules to " + DescribePolicy(*a.Policy)
	case treasuryabi.SetMember:
		if a.Member.Weight == 0 {
			return "remove " + a.Member.Address + " as a voter"
		}
		return fmt.Sprintf("give %s a vote of weight %d", a.Member.Address, a.Member.Weight)
	case treasuryabi.AllowRecipient:
		return "allow payments to " + a.Recipient
	case treasuryabi.DenyRecipient:
		return "stop payments to " + a.Recipient
	case treasuryabi.Revoke:
		return fmt.Sprintf("stop proposal %d", a.Target)
	default:
		return "do something this deployment cannot describe"
	}
}

// DescribePolicy writes a policy on one line.
func DescribePolicy(p treasuryabi.Policy) string {
	spend := "no spending without a proposal"
	if p.SpendLimit != nil && p.SpendLimit.Sign() > 0 {
		spend = fmt.Sprintf("up to %s without a proposal every %s",
			TokenAmount(p.SpendLimit), Span(p.SpendWindow))
	}
	return fmt.Sprintf(
		"quorum %s, approval %s, voting open %s, timelock %s, %s",
		percent(p.QuorumBps), percent(p.ApprovalBps),
		Span(p.VotingPeriod), Span(p.Timelock), spend)
}

func percent(bps uint32) string {
	if bps%100 == 0 {
		return fmt.Sprintf("%d%%", bps/100)
	}
	return fmt.Sprintf("%d.%02d%%", bps/100, bps%100)
}

// Span writes a number of seconds the way somebody would say it.
func Span(seconds uint64) string {
	if seconds == 0 {
		return "none"
	}
	var parts []string
	for _, unit := range []struct {
		size uint64
		name string
	}{{86_400, "d"}, {3_600, "h"}, {60, "m"}, {1, "s"}} {
		if n := seconds / unit.size; n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, unit.name))
			seconds %= unit.size
		}
	}
	return strings.Join(parts, " ")
}

// CreateTreasuryParams is a contract treasury to bring into existence.
type CreateTreasuryParams struct {
	Label      string
	Policy     treasuryabi.Policy
	Members    []treasuryabi.Member
	Recipients []string
	By         store.IdentityID
}

// CreateContractTreasury creates a treasury contract and records it.
//
// The relay account creates it and gets nothing for doing so: the contract has
// no admin, and everything it starts with — its rules, its members, whom it
// may pay — is in the one transaction that creates it. After that, none of it
// changes except by its members' vote.
//
// What comes back is read from the new contract and not echoed from the
// request, so that what a workspace is told it has is what it has.
func (s *Service) CreateContractTreasury(
	ctx context.Context, scope Scope, p CreateTreasuryParams,
) (*TreasuryState, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if len(p.Members) == 0 {
		return nil, refusef("a treasury needs at least one member with a wallet")
	}

	policy, err := p.Policy.ScVal()
	if err != nil {
		return nil, err
	}
	members, err := treasuryabi.Members(p.Members)
	if err != nil {
		return nil, err
	}
	recipients, err := treasuryabi.Addresses(p.Recipients)
	if err != nil {
		return nil, err
	}

	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("api: salt for a new treasury: %w", err)
	}

	relaying.Lock()
	defer relaying.Unlock()

	prepared, err := s.settle.PrepareCreate(ctx, settlement.CreateRequest{
		Source:      cfg.RelayAddress,
		WasmHash:    cfg.WasmHash,
		Salt:        salt,
		Constructor: []xdr.ScVal{policy, members, recipients},
	})
	if err != nil {
		return nil, refusalOf(err)
	}
	signed, err := cfg.SignRelay(prepared.Transaction)
	if err != nil {
		return nil, fmt.Errorf("api: sign the creation of a treasury: %w", err)
	}
	result, err := s.settle.SubmitSoroban(ctx, signed)
	if err != nil {
		return nil, err
	}
	if result.Failed {
		return nil, fmt.Errorf("%w: creating the treasury (transaction %s)", ErrCallFailedOnChain, result.Hash)
	}

	treasury, err := s.store.AddContractTreasury(ctx, scope.Org, prepared.Address, p.Label, p.By)
	if err != nil {
		// It exists and this deployment has no record of it. Said with the
		// address, because that is the only handle anybody has on it now.
		return nil, fmt.Errorf("api: treasury %s was created (transaction %s) and could not be recorded: %w",
			prepared.Address, result.Hash, err)
	}
	return s.treasuryState(ctx, cfg, treasury)
}

// AdoptContractTreasury records a treasury contract that already exists.
//
// Two things are checked and both are read from the network. That the
// contract runs the treasury code this deployment was built with — the
// address alone says nothing about what is behind it. And that whoever is
// asking is one of its members, by the contract's own list: a workspace has no
// business with a treasury none of its people has a vote in.
func (s *Service) AdoptContractTreasury(
	ctx context.Context, scope Scope, address, label string, member store.Member, by store.IdentityID,
) (*TreasuryState, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if !strkey.IsValidContractAddress(address) {
		return nil, refusef("%q is not a contract address", address)
	}
	if !member.HasAddress() {
		return nil, ErrNoWallet
	}

	exe, err := s.settle.ContractExecutable(ctx, address)
	if err != nil {
		return nil, err
	}
	if exe.Kind != "wasm" || exe.WasmHash != cfg.WasmHash {
		return nil, fmt.Errorf("%w: %s runs %s", ErrNotTreasuryCode, address, exe)
	}
	v, err := s.readTreasury(ctx, cfg, address, "version")
	if err != nil {
		return nil, err
	}
	if version, err := treasuryabi.AsU32(v); err != nil || version != treasuryabi.Version {
		return nil, fmt.Errorf("%w: %s answers version %d", ErrNotTreasuryCode, address, version)
	}

	weight, err := s.memberWeight(ctx, cfg, address, member.Address)
	if err != nil {
		return nil, err
	}
	if weight == 0 {
		return nil, &ContractRefusal{Code: treasuryabi.NotMember}
	}

	treasury, err := s.store.AddContractTreasury(ctx, scope.Org, address, label, by)
	if err != nil {
		return nil, err
	}
	return s.treasuryState(ctx, cfg, treasury)
}

// PreparedCall is a call built for one member, waiting for their signature.
type PreparedCall struct {
	Hash      string
	ExpiresAt time.Time
}

// memberCall is one call to build.
type memberCall struct {
	Treasury store.Treasury
	Member   store.Member
	// Contract is what is called: the treasury, or for a deposit the token.
	Contract string
	Function string
	Args     []xdr.ScVal
	Says     string
	Note     string
	Memo     []byte
}

// prepareMemberCall builds a call from a member's own address, proves the
// contract would accept it, and records it for that member to sign.
//
// Simulated before anybody is asked for anything. A member who is not one, a
// vote on a proposal that has closed, a payment to somebody not on the list:
// each is refused here, by the contract, for nothing — and not on chain, after
// a signature, for a sequence number.
func (s *Service) prepareMemberCall(ctx context.Context, scope Scope, c memberCall) (*PreparedCall, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	if _, err := s.contractTreasuriesOn(); err != nil {
		return nil, err
	}
	if c.Treasury.Kind != store.TreasuryContract || c.Treasury.Org != scope.Org {
		return nil, ErrNotContractTreasury
	}
	if !c.Member.HasAddress() {
		return nil, ErrNoWallet
	}

	prepared, err := s.settle.PrepareCall(ctx, settlement.CallRequest{
		Source:   c.Member.Address,
		Contract: c.Contract,
		Function: c.Function,
		Args:     c.Args,
		Timeout:  memberCallLifetime,
	})
	if err != nil {
		var problem *horizonclient.Error
		if errors.As(err, &problem) && problem.Problem.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrWalletNotOnChain, c.Member.Address)
		}
		if c.Contract != c.Treasury.Address {
			if code, ok := settlement.ContractErrorCode(err); ok {
				return nil, &TokenRefusal{Code: code}
			}
			return nil, err
		}
		return nil, refusalOf(err)
	}

	hash, err := prepared.Transaction.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash call: %w", err)
	}
	envelope, err := prepared.Transaction.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode call: %w", err)
	}
	expiresAt := time.Unix(prepared.Transaction.Timebounds().MaxTime, 0).UTC()

	if err := s.store.SaveMemberCall(ctx, store.MemberCall{
		Hash: hash, Org: scope.Org, OwnerRef: scope.OwnerRef,
		Member: c.Member.ID, Treasury: c.Treasury.ID,
		Source: c.Member.Address, Contract: c.Contract, Function: c.Function,
		XDR: envelope, Says: c.Says, Note: c.Note, Memo: c.Memo,
		ExpiresAt: expiresAt,
	}); err != nil {
		return nil, err
	}
	return &PreparedCall{Hash: hash, ExpiresAt: expiresAt}, nil
}

// ProposeOnChain builds the call that puts something to a treasury's members.
func (s *Service) ProposeOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, member store.Member,
	action treasuryabi.Action, note string,
) (*PreparedCall, error) {
	note = strings.TrimSpace(note)
	memo := noteMemo(note)

	who, err := memberArg(member)
	if err != nil {
		return nil, err
	}
	what, err := action.ScVal()
	if err != nil {
		return nil, err
	}
	return s.prepareMemberCall(ctx, scope, memberCall{
		Treasury: treasury, Member: member,
		Contract: treasury.Address, Function: "propose",
		Args: []xdr.ScVal{who, what, treasuryabi.Memo(memo)},
		Says: "Propose: " + s.DescribeAction(action),
		Note: note, Memo: memo[:],
	})
}

// VoteOnChain builds a member's vote on a proposal.
func (s *Service) VoteOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, member store.Member,
	proposal *ChainProposal, approve bool,
) (*PreparedCall, error) {
	who, err := memberArg(member)
	if err != nil {
		return nil, err
	}
	vote := "against"
	if approve {
		vote = "for"
	}
	return s.prepareMemberCall(ctx, scope, memberCall{
		Treasury: treasury, Member: member,
		Contract: treasury.Address, Function: "vote",
		Args: []xdr.ScVal{who, treasuryabi.U32(proposal.ID), treasuryabi.Bool(approve)},
		Says: fmt.Sprintf("Vote %s proposal %d, which is to %s",
			vote, proposal.ID, s.DescribeAction(proposal.Action)),
	})
}

// WithdrawOnChain builds the call by which a proposer withdraws a proposal.
func (s *Service) WithdrawOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, member store.Member,
	proposal *ChainProposal,
) (*PreparedCall, error) {
	who, err := memberArg(member)
	if err != nil {
		return nil, err
	}
	return s.prepareMemberCall(ctx, scope, memberCall{
		Treasury: treasury, Member: member,
		Contract: treasury.Address, Function: "cancel",
		Args: []xdr.ScVal{who, treasuryabi.U32(proposal.ID)},
		Says: fmt.Sprintf("Withdraw proposal %d, which is to %s",
			proposal.ID, s.DescribeAction(proposal.Action)),
	})
}

// SpendOnChain builds a payment under the rolling allowance, which one member
// makes alone.
func (s *Service) SpendOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, member store.Member,
	asset txnbuild.Asset, to string, amount money.Stroops,
) (*PreparedCall, error) {
	who, err := memberArg(member)
	if err != nil {
		return nil, err
	}
	token, tokenArg, err := s.tokenOf(asset)
	if err != nil {
		return nil, err
	}
	recipient, err := treasuryabi.Address(to)
	if err != nil {
		return nil, refusef("%q is not somewhere a payment can go", to)
	}
	sum, err := treasuryabi.Amount(big.NewInt(int64(amount)))
	if err != nil {
		return nil, err
	}
	return s.prepareMemberCall(ctx, scope, memberCall{
		Treasury: treasury, Member: member,
		Contract: treasury.Address, Function: "spend",
		Args: []xdr.ScVal{who, tokenArg, recipient, sum},
		Says: fmt.Sprintf("Spend %s %s from the treasury to %s, without a proposal",
			amount.Display(), s.TokenName(token), to),
	})
}

// DepositOnChain builds a transfer from a member's own wallet into a treasury.
//
// The one call here that is not to the treasury. A contract is paid through
// the token's contract, so this is that token's `transfer`, from the member, to
// the treasury — and the money it moves is the member's own.
func (s *Service) DepositOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, member store.Member,
	asset txnbuild.Asset, amount money.Stroops,
) (*PreparedCall, error) {
	who, err := memberArg(member)
	if err != nil {
		return nil, err
	}
	token, _, err := s.tokenOf(asset)
	if err != nil {
		return nil, err
	}
	into, err := treasuryabi.Address(treasury.Address)
	if err != nil {
		return nil, err
	}
	sum, err := treasuryabi.Amount(big.NewInt(int64(amount)))
	if err != nil {
		return nil, err
	}
	return s.prepareMemberCall(ctx, scope, memberCall{
		Treasury: treasury, Member: member,
		Contract: token, Function: "transfer",
		Args: []xdr.ScVal{who, into, sum},
		Says: fmt.Sprintf("Deposit %s %s from your wallet into the treasury",
			amount.Display(), s.AssetName(asset)),
	})
}

func memberArg(member store.Member) (xdr.ScVal, error) {
	if !member.HasAddress() {
		return xdr.ScVal{}, ErrNoWallet
	}
	return treasuryabi.Address(member.Address)
}

// tokenOf is the contract of a classic asset, as an address and as an
// argument.
func (s *Service) tokenOf(asset txnbuild.Asset) (string, xdr.ScVal, error) {
	token, err := s.settle.AssetContract(asset)
	if err != nil {
		return "", xdr.ScVal{}, err
	}
	arg, err := treasuryabi.Address(token)
	return token, arg, err
}

// PayAction is the action that pays an amount of a classic asset out of a
// treasury.
func (s *Service) PayAction(asset txnbuild.Asset, to string, amount money.Stroops) (treasuryabi.Action, error) {
	token, _, err := s.tokenOf(asset)
	if err != nil {
		return treasuryabi.Action{}, err
	}
	return treasuryabi.Action{Kind: treasuryabi.Pay, Payment: &treasuryabi.Payment{
		Token: token, To: to, Amount: big.NewInt(int64(amount)),
	}}, nil
}

// TreasuryAsset reads what somebody called an asset, for a contract treasury:
// XLM, the deployment's own asset by its code, or CODE:ISSUER.
func (s *Service) TreasuryAsset(text string) (txnbuild.Asset, error) {
	if strings.TrimSpace(text) == "" {
		return s.cfg.Asset, nil
	}
	return s.parseAsset(text, nil)
}

// MemberCallView is what a member is shown before they sign a call.
type MemberCallView struct {
	Call        store.MemberCall
	Treasury    store.Treasury
	Description *settlement.TxDescription
	// Assets are the classic assets a token in this call might be. The page
	// works out each one's contract for itself and names a token only where
	// its own answer matches.
	Assets []txnbuild.Asset
}

// LoadMemberCall returns a call that is still waiting for its member to sign.
func (s *Service) LoadMemberCall(ctx context.Context, scope Scope, hash string) (*MemberCallView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	call, err := s.store.MemberCall(ctx, scope.Org, scope.OwnerRef, hash)
	if err != nil {
		return nil, err
	}
	if call.SubmittedAt != nil {
		return nil, fmt.Errorf("%w: %s", store.ErrCallSubmitted, hash)
	}
	if !time.Now().Before(call.ExpiresAt) {
		return nil, fmt.Errorf("%w: %s", store.ErrCallExpired, hash)
	}

	treasury, err := s.store.Treasury(ctx, scope.Org, call.Treasury)
	if err != nil {
		return nil, err
	}

	parsed, err := txnbuild.TransactionFromXDR(call.XDR)
	if err != nil {
		return nil, fmt.Errorf("api: stored call %s is unreadable: %w", hash, err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		return nil, fmt.Errorf("api: stored call %s is not a plain transaction", hash)
	}
	// Described from the envelope, like everything else a page is handed. The
	// stored hash is what the link authorises, and the envelope must still
	// hash to it.
	described, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	if described.Hash != hash {
		return nil, fmt.Errorf("api: stored call hashes to %s, expected %s", described.Hash, hash)
	}

	assets := []txnbuild.Asset{txnbuild.NativeAsset{}}
	if s.cfg.Asset != nil && !s.cfg.Asset.IsNative() {
		assets = append(assets, s.cfg.Asset)
	}
	return &MemberCallView{Call: call, Treasury: treasury, Description: described, Assets: assets}, nil
}

// MemberCallResult is what became of a call a member signed.
type MemberCallResult struct {
	// Hash is what landed: the fee bump around the member's call.
	Hash   string
	Ledger int32
	// Function is what was called.
	Function string
	// Proposal is the number the contract gave the proposal this call opened,
	// when it opened one and the number could be read back.
	Proposal uint32
}

// SubmitMemberCall sends a call its member has signed.
//
// Accepted only if it is a call this server built, for whoever is submitting
// it, that has not been sent before. The operator pays the fee, so the member
// needs no XLM to take part; the signature inside is still theirs, over
// exactly what they were shown.
func (s *Service) SubmitMemberCall(
	ctx context.Context, scope Scope, signedXDR, feeAccount string,
	sign func(*txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error),
) (*MemberCallResult, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	if _, err := s.contractTreasuriesOn(); err != nil {
		return nil, err
	}

	parsed, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable envelope", ErrUnsigned)
	}
	inner, ok := parsed.Transaction()
	if !ok {
		// The envelope that pays is the operator's to build.
		return nil, fmt.Errorf("%w: expected the call itself, not a fee bump", ErrUnsigned)
	}
	if len(inner.Signatures()) == 0 {
		return nil, ErrUnsigned
	}
	hash, err := inner.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash call: %w", err)
	}

	call, err := s.store.ClaimMemberCall(ctx, scope.Org, scope.OwnerRef, hash)
	if err != nil {
		return nil, err
	}

	bump, err := s.settle.FeeBumpSoroban(inner, feeAccount)
	if err != nil {
		_ = s.store.ReleaseMemberCall(ctx, scope.Org, hash)
		return nil, err
	}
	signedBump, err := sign(bump)
	if err != nil {
		_ = s.store.ReleaseMemberCall(ctx, scope.Org, hash)
		return nil, fmt.Errorf("api: sign fee-bump: %w", err)
	}

	res, err := s.settle.SubmitSorobanFeeBump(ctx, signedBump)
	switch {
	case errors.Is(err, settlement.ErrRejected), errors.Is(err, settlement.ErrTryAgainLater):
		// Turned away before it reached a ledger: nothing was spent, and the
		// same envelope may be offered again — with a signature that verifies,
		// if that was what was wrong with it.
		if releaseErr := s.store.ReleaseMemberCall(ctx, scope.Org, hash); releaseErr != nil {
			return nil, releaseErr
		}
		return nil, err
	case err != nil:
		// Not known. The claim stays, so that an outcome nobody established
		// cannot be paid for twice.
		return nil, err
	}

	out := &MemberCallResult{Hash: res.Hash, Ledger: res.Ledger, Function: call.Function}
	if !res.Failed && call.Function == "propose" && call.Contract != "" {
		out.Proposal = returnedProposal(res.ReturnValue)
	}
	if err := s.store.LandMemberCall(ctx, scope.Org, hash, res.Hash, res.Ledger, res.Failed, out.Proposal); err != nil {
		return nil, err
	}
	if res.Failed {
		return nil, fmt.Errorf("%w: transaction %s", ErrCallFailedOnChain, res.Hash)
	}
	return out, nil
}

// returnedProposal reads the number a `propose` call returned. Zero when it
// cannot be read: the proposal exists either way, and its number can be found
// by listing.
func returnedProposal(returnValue string) uint32 {
	if returnValue == "" {
		return 0
	}
	var v xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(returnValue, &v); err != nil {
		return 0
	}
	id, err := treasuryabi.AsU32(v)
	if err != nil {
		return 0
	}
	return id
}

// relay sends one call to a treasury that needs nobody's signature.
func (s *Service) relay(
	ctx context.Context, cfg *ContractTreasuryConfig, contract, function string, args ...xdr.ScVal,
) (settlement.Result, error) {
	relaying.Lock()
	defer relaying.Unlock()

	prepared, err := s.settle.PrepareCall(ctx, settlement.CallRequest{
		Source: cfg.RelayAddress, Contract: contract, Function: function, Args: args,
	})
	if err != nil {
		return settlement.Result{}, refusalOf(err)
	}
	signed, err := cfg.SignRelay(prepared.Transaction)
	if err != nil {
		return settlement.Result{}, fmt.Errorf("api: sign %s: %w", function, err)
	}
	res, err := s.settle.SubmitSoroban(ctx, signed)
	if err != nil {
		return settlement.Result{}, err
	}
	if res.Failed {
		return res, fmt.Errorf("%w: %s (transaction %s)", ErrCallFailedOnChain, function, res.Hash)
	}
	return res, nil
}

// ExecuteOnChain carries out a proposal the members have passed.
//
// Anyone may, by the contract's design: the decision was made when the votes
// were cast, and what a proposal does was fixed when it was written. This
// deployment does it so that nobody has to hold XLM to press a button, and the
// contract would take the same call from anybody else if this deployment were
// gone.
func (s *Service) ExecuteOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, id uint32,
) (settlement.Result, error) {
	cfg, err := s.relayFor(scope, treasury)
	if err != nil {
		return settlement.Result{}, err
	}
	return s.relay(ctx, cfg, treasury.Address, "execute", treasuryabi.U32(id))
}

// SettleOnChain records the outcome of a proposal whose outcome is no longer
// in question, and reports it.
func (s *Service) SettleOnChain(
	ctx context.Context, scope Scope, treasury store.Treasury, id uint32,
) (string, error) {
	cfg, err := s.relayFor(scope, treasury)
	if err != nil {
		return "", err
	}
	res, err := s.relay(ctx, cfg, treasury.Address, "tally", treasuryabi.U32(id))
	if err != nil {
		return "", err
	}
	var v xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(res.ReturnValue, &v); err != nil {
		// It was recorded; what was recorded is read back instead.
		p, readErr := s.chainProposal(ctx, cfg, treasury, id)
		if readErr != nil {
			return "", readErr
		}
		return p.Status, nil
	}
	return treasuryabi.AsStatus(v)
}

func (s *Service) relayFor(scope Scope, treasury store.Treasury) (*ContractTreasuryConfig, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.writable(); err != nil {
		return nil, err
	}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryContract || treasury.Org != scope.Org {
		return nil, ErrNotContractTreasury
	}
	return cfg, nil
}

// contractBalance reads what a contract treasury holds of the assets this
// deployment can name.
//
// A contract has no list of balances to read the way an account does: each
// token keeps its own books. So the question is asked of each token in turn,
// and only of the ones this deployment knows. A token it does not know of is
// not shown as zero; it is not shown.
func (s *Service) contractBalance(ctx context.Context, treasury store.Treasury) (AccountBalance, error) {
	out := AccountBalance{Label: treasury.Label, Address: treasury.Address, Exists: true, Contract: true}
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		// A treasury recorded when this was on, read on a deployment where it
		// is off. Named, with nothing under it.
		return out, nil
	}
	holder, err := treasuryabi.Address(treasury.Address)
	if err != nil {
		return out, err
	}

	assets := []txnbuild.Asset{txnbuild.NativeAsset{}}
	if s.cfg.Asset != nil && !s.cfg.Asset.IsNative() {
		assets = append(assets, s.cfg.Asset)
	}
	for _, asset := range assets {
		token, err := s.settle.AssetContract(asset)
		if err != nil {
			return out, err
		}
		v, err := s.settle.Read(ctx, settlement.CallRequest{
			Source: cfg.RelayAddress, Contract: token, Function: "balance", Args: []xdr.ScVal{holder},
		})
		if errors.Is(err, settlement.ErrSimulationFailed) {
			// The asset has no contract on this network yet, so nothing can
			// hold it through one.
			continue
		}
		if err != nil {
			return out, err
		}
		amount, err := treasuryabi.AsAmount(v)
		if err != nil {
			return out, err
		}
		if !amount.IsInt64() {
			return out, fmt.Errorf("api: %s holds more %s than can be shown", treasury.Address, s.AssetName(asset))
		}
		out.Lines = append(out.Lines, BalanceLine{
			Asset: s.AssetName(asset), Amount: money.Stroops(amount.Int64()), Authorized: true,
		})
	}
	return out, nil
}
