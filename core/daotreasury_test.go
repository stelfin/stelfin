package core

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/contracts/treasuryabi"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// chainFake stands in for a treasury contract, as far as the commands can see
// one: the proposals it holds, what it was asked, and what it refuses.
//
// It does none of the contract's arithmetic. What is under test here is what
// the bot says and builds for each state a contract can be in, and the
// contract's own suite is what proves the states are reached correctly.
type chainFake struct {
	proposals map[uint32]*api.ChainProposal
	// refuse is what the next call that would build something returns.
	refuse error

	created   []api.CreateTreasuryParams
	adopted   []string
	proposed  []treasuryabi.Action
	notes     []string
	votes     []string
	withdrawn []uint32
	spent     []string
	deposited []string
	executed  []uint32
	settled   []uint32
	calls     int
}

func someContract() string {
	raw := keypair.MustRandom().Address()
	decoded, _ := strkey.Decode(strkey.VersionByteAccountID, raw)
	address, _ := strkey.Encode(strkey.VersionByteContract, decoded)
	return address
}

// WaitingCalls is how many calls the fake has handed out. None of them is
// ever sent, so all of them are waiting.
func (f *fakeSender) WaitingCalls(context.Context, api.Scope) (int, error) {
	return f.chain.calls, nil
}

func (f *fakeSender) prepared() (*api.PreparedCall, error) {
	if f.chain.refuse != nil {
		return nil, f.chain.refuse
	}
	f.chain.calls++
	return &api.PreparedCall{
		Hash: fmt.Sprintf("call%d", f.chain.calls), ExpiresAt: time.Now().Add(10 * time.Minute),
	}, nil
}

func (f *fakeSender) state(treasury store.Treasury) *api.TreasuryState {
	return &api.TreasuryState{
		Treasury: treasury,
		Policy: treasuryabi.Policy{
			QuorumBps: 5_000, ApprovalBps: 6_000, VotingPeriod: 259_200, Timelock: 86_400,
			SpendLimit: big.NewInt(0),
		},
		Spendable: big.NewInt(0),
	}
}

func (f *fakeSender) CreateContractTreasury(
	ctx context.Context, scope api.Scope, p api.CreateTreasuryParams,
) (*api.TreasuryState, error) {
	if f.chain.refuse != nil {
		return nil, f.chain.refuse
	}
	f.chain.created = append(f.chain.created, p)
	treasury, err := f.db.AddContractTreasury(ctx, scope.Org, someContract(), p.Label, p.By)
	if err != nil {
		return nil, err
	}
	state := f.state(treasury)
	state.Policy, state.Members, state.Recipients = p.Policy, p.Members, p.Recipients
	for _, m := range p.Members {
		state.TotalWeight += m.Weight
	}
	return state, nil
}

func (f *fakeSender) AdoptContractTreasury(
	ctx context.Context, scope api.Scope, address, label string, _ store.Member, by store.IdentityID,
) (*api.TreasuryState, error) {
	if f.chain.refuse != nil {
		return nil, f.chain.refuse
	}
	f.chain.adopted = append(f.chain.adopted, address)
	treasury, err := f.db.AddContractTreasury(ctx, scope.Org, address, label, by)
	if err != nil {
		return nil, err
	}
	return f.state(treasury), nil
}

func (f *fakeSender) ContractTreasuryState(
	_ context.Context, _ api.Scope, treasury store.Treasury,
) (*api.TreasuryState, error) {
	if f.chain.refuse != nil {
		return nil, f.chain.refuse
	}
	return f.state(treasury), nil
}

func (f *fakeSender) ChainProposals(
	_ context.Context, _ api.Scope, _ store.Treasury, _ int,
) ([]api.ChainProposal, error) {
	var out []api.ChainProposal
	for _, p := range f.chain.proposals {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (f *fakeSender) ChainProposalByID(
	_ context.Context, _ api.Scope, _ store.Treasury, id uint32,
) (*api.ChainProposal, error) {
	p, ok := f.chain.proposals[id]
	if !ok {
		return nil, &api.ContractRefusal{Code: treasuryabi.UnknownProposal}
	}
	copied := *p
	return &copied, nil
}

func (f *fakeSender) ProposeOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, _ store.Member, action treasuryabi.Action, note string,
) (*api.PreparedCall, error) {
	prepared, err := f.prepared()
	if err == nil {
		f.chain.proposed = append(f.chain.proposed, action)
		f.chain.notes = append(f.chain.notes, note)
	}
	return prepared, err
}

func (f *fakeSender) VoteOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, _ store.Member, p *api.ChainProposal, approve bool,
) (*api.PreparedCall, error) {
	prepared, err := f.prepared()
	if err == nil {
		f.chain.votes = append(f.chain.votes, fmt.Sprintf("%d:%v", p.ID, approve))
	}
	return prepared, err
}

func (f *fakeSender) WithdrawOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, _ store.Member, p *api.ChainProposal,
) (*api.PreparedCall, error) {
	prepared, err := f.prepared()
	if err == nil {
		f.chain.withdrawn = append(f.chain.withdrawn, p.ID)
	}
	return prepared, err
}

func (f *fakeSender) SpendOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, _ store.Member,
	asset txnbuild.Asset, to string, amount money.Stroops,
) (*api.PreparedCall, error) {
	prepared, err := f.prepared()
	if err == nil {
		f.chain.spent = append(f.chain.spent, fmt.Sprintf("%s %s to %s", amount, asset.GetCode(), to))
	}
	return prepared, err
}

func (f *fakeSender) DepositOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, _ store.Member, asset txnbuild.Asset, amount money.Stroops,
) (*api.PreparedCall, error) {
	prepared, err := f.prepared()
	if err == nil {
		code := asset.GetCode()
		if asset.IsNative() {
			code = "XLM"
		}
		f.chain.deposited = append(f.chain.deposited, fmt.Sprintf("%s %s", amount, code))
	}
	return prepared, err
}

func (f *fakeSender) ExecuteOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, id uint32,
) (settlement.Result, error) {
	if f.chain.refuse != nil {
		return settlement.Result{}, f.chain.refuse
	}
	f.chain.executed = append(f.chain.executed, id)
	return settlement.Result{Hash: "feedface", Ledger: 77}, nil
}

// SettleOnChain records what settling would record, as the contract does.
func (f *fakeSender) SettleOnChain(
	_ context.Context, _ api.Scope, _ store.Treasury, id uint32,
) (string, error) {
	f.chain.settled = append(f.chain.settled, id)
	p := f.chain.proposals[id]
	p.Status, p.Settles = p.Settles, ""
	return p.Status, nil
}

func (f *fakeSender) PayAction(_ txnbuild.Asset, to string, amount money.Stroops) (treasuryabi.Action, error) {
	return treasuryabi.Action{Kind: treasuryabi.Pay, Payment: &treasuryabi.Payment{
		Token: "CTOKEN", To: to, Amount: big.NewInt(int64(amount)),
	}}, nil
}

func (f *fakeSender) TreasuryAsset(text string) (txnbuild.Asset, error) {
	switch strings.ToUpper(text) {
	case "", "USDC":
		return txnbuild.CreditAsset{Code: "USDC", Issuer: "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"}, nil
	case "XLM":
		return txnbuild.NativeAsset{}, nil
	}
	return nil, &api.RefusalError{Reason: "I don't know which " + text + " you mean"}
}

func (f *fakeSender) DescribeAction(a treasuryabi.Action) string {
	switch a.Kind {
	case treasuryabi.Pay:
		return fmt.Sprintf("pay %s to %s", api.TokenAmount(a.Payment.Amount), a.Payment.To)
	case treasuryabi.Revoke:
		return fmt.Sprintf("stop proposal %d", a.Target)
	case treasuryabi.SetMember:
		return fmt.Sprintf("give %s a vote of weight %d", a.Member.Address, a.Member.Weight)
	case treasuryabi.SetPolicy:
		return "change the rules to " + api.DescribePolicy(*a.Policy)
	default:
		return strings.ToLower(a.Kind) + " " + a.Recipient
	}
}

// heldByContract sets a workspace up with one treasury held by a contract, and
// the member who speaks in these tests as a proposer with a wallet.
func heldByContract(t *testing.T, h *harness, space string) (store.Org, store.Treasury, string) {
	t.Helper()
	ctx := context.Background()
	org := setUpWorkspace(t, h, space)
	treasury, err := h.store.AddContractTreasury(ctx, org.ID, someContract(), "ops", 0)
	if err != nil {
		t.Fatalf("add treasury: %v", err)
	}
	id := grantRole(t, h, org, "42", chat.RoleProposer)
	wallet := keypair.MustRandom().Address()
	if err := h.store.SetMemberAddress(ctx, org.ID, id, wallet, store.AddressLinked); err != nil {
		t.Fatalf("set address: %v", err)
	}
	h.sender.chain.proposals = map[uint32]*api.ChainProposal{}
	h.out = &fakeReplier{}
	return org, treasury, wallet
}

func (h *harness) say(t *testing.T, space, command, args string) string {
	t.Helper()
	h.out = &fakeReplier{}
	if err := h.handle(t, message(space, command, args)); err != nil {
		t.Fatalf("/%s %s: %v", command, args, err)
	}
	return h.out.only(t).Text
}

func mentions(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}
}

func open(id uint32, proposer string, yes, no uint32) *api.ChainProposal {
	return &api.ChainProposal{Proposal: treasuryabi.Proposal{
		ID: id, Proposer: proposer, Status: treasuryabi.Open,
		Action: treasuryabi.Action{Kind: treasuryabi.Pay, Payment: &treasuryabi.Payment{
			Token: "CTOKEN", To: "GPAYEE", Amount: big.NewInt(2_500_000_000),
		}},
		ClosesAt:     uint64(time.Now().Add(48 * time.Hour).Unix()),
		ExecutableAt: uint64(time.Now().Add(72 * time.Hour).Unix()),
		Yes:          yes, No: no,
	}}
}

// The voters a new treasury starts with are the people here who may approve
// and have somewhere to vote from. Nobody else, and nobody twice.
func TestANewTreasurysVotersAreTheApproversWithWallets(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org := setUpWorkspace(t, h, space)
	ctx := context.Background()

	wallets := map[string]string{}
	for user, role := range map[string]chat.Role{
		"42": chat.RoleAdmin, "50": chat.RoleApprover, "51": chat.RoleApprover, "52": chat.RoleObserver,
	} {
		id := grantRole(t, h, org, user, role)
		if user == "51" {
			continue // an approver who has not linked a wallet
		}
		wallets[user] = keypair.MustRandom().Address()
		if err := h.store.SetMemberAddress(ctx, org.ID, id, wallets[user], store.AddressLinked); err != nil {
			t.Fatalf("set address: %v", err)
		}
	}

	payee := keypair.MustRandom().Address()
	body := h.say(t, space, "create-treasury", "ops timelock:12h approval:75 recipients:"+payee)

	if len(h.sender.chain.created) != 1 {
		t.Fatalf("created %d treasuries:\n%s", len(h.sender.chain.created), body)
	}
	made := h.sender.chain.created[0]
	var voters []string
	for _, m := range made.Members {
		if m.Weight != 1 {
			t.Errorf("%s starts with weight %d", m.Address, m.Weight)
		}
		voters = append(voters, m.Address)
	}
	sort.Strings(voters)
	want := []string{wallets["42"], wallets["50"]}
	sort.Strings(want)
	if fmt.Sprint(voters) != fmt.Sprint(want) {
		t.Errorf("voters %v, want the admin and the approver with a wallet %v", voters, want)
	}
	if made.Policy.Timelock != 12*3600 || made.Policy.ApprovalBps != 7_500 || made.Policy.QuorumBps != 5_000 {
		t.Errorf("rules %+v", made.Policy)
	}
	if len(made.Recipients) != 1 || made.Recipients[0] != payee {
		t.Errorf("recipients %v", made.Recipients)
	}
	mentions(t, body, "held by a contract", "approval 75%", "timelock 12h",
		// The approver without a wallet is said to have been left out.
		"1 approver(s) here had no wallet", "Nobody stands above its voters")

	// A second by the same name is a second thing called the same.
	if body := h.say(t, space, "create-treasury", "OPS"); !strings.Contains(body, "already a treasury called") {
		t.Errorf("a second treasury of the same name:\n%s", body)
	}
}

func TestATreasuryNobodyCouldVoteInIsNotCreated(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	setUpWorkspace(t, h, space)

	body := h.say(t, space, "create-treasury", "ops")
	if len(h.sender.chain.created) != 0 {
		t.Fatal("created a treasury with no voters")
	}
	mentions(t, body, "Nobody here could vote in it yet", "/link")

	for args, want := range map[string]string{
		"":                 "What should it be called?",
		"ops timelock:1":   "length of time",
		"ops quorum:0":     "percentage",
		"ops approval:abc": "percentage",
		"ops recipients:x": "isn't somewhere a payment can go",
		"9lives":           "short word",
	} {
		if body := h.say(t, space, "create-treasury", args); !strings.Contains(body, want) {
			t.Errorf("/create-treasury %s:\n%s", args, body)
		}
	}
}

// /pay on a treasury held by a contract builds a proposal for its voters and
// hands the proposer a link to their own call. It opens no envelope for
// signatures, because there is none to collect.
func TestPayFromAContractTreasuryIsACallTheProposerSigns(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org, _, _ := heldByContract(t, h, space)
	to := keypair.MustRandom().Address()

	body := h.say(t, space, "pay", "250 to "+to+" asset:XLM for the audit")
	mentions(t, body, "/call#call1", "ops's voters", "pay 250.00 to "+to, "only a proposal", "stelfin pays the fee")
	if strings.Contains(body, "/approve#") {
		t.Errorf("a contract treasury's payment was offered for signatures:\n%s", body)
	}

	if len(h.sender.chain.proposed) != 1 {
		t.Fatalf("proposed %d actions", len(h.sender.chain.proposed))
	}
	got := h.sender.chain.proposed[0]
	if got.Kind != treasuryabi.Pay || got.Payment.To != to || got.Payment.Amount.Int64() != int64(money.MustParse("250")) {
		t.Errorf("proposed %+v", got.Payment)
	}
	// The asset is not part of the note; what follows the address is.
	if note := h.sender.chain.notes[0]; note != "for the audit" {
		t.Errorf("note %q", note)
	}
	// And no row was opened in the table classic proposals live in.
	if list, _ := h.store.Proposals(context.Background(), org.ID, 10); len(list) != 0 {
		t.Errorf("a contract treasury's proposal was written as a classic one: %+v", list)
	}
}

// What the contract refuses is said in terms of what to do about it.
func TestAContractsRefusalIsExplained(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	_, _, wallet := heldByContract(t, h, space)
	to := keypair.MustRandom().Address()

	for code, wants := range map[treasuryabi.Code][]string{
		treasuryabi.RecipientNotAllowed: {"isn't one ops may pay", "/allow"},
		treasuryabi.NotMember:           {wallet, "has no vote in ops", "/voter"},
		treasuryabi.InvalidAmount:       {"more than nothing"},
		treasuryabi.Code(4242):          {"4242"},
	} {
		h.sender.chain.refuse = &api.ContractRefusal{Code: code}
		body := h.say(t, space, "pay", "250 to "+to)
		mentions(t, body, wants...)
		if strings.Contains(body, "/call#") {
			t.Errorf("a refused call was still handed out:\n%s", body)
		}
	}

	for refusal, want := range map[error]string{
		api.ErrWalletNotOnChain:      "not on the network yet",
		api.ErrContractTreasuriesOff: "no Soroban RPC",
		&api.TokenRefusal{Code: 10}:  "token wouldn't make that transfer",
		// The network's own, each of which would otherwise be "something
		// went wrong" every time it was tried.
		fmt.Errorf("%w: HostError: Error(Storage, MissingValue)", settlement.ErrSimulationFailed): "may have no contract on this network yet",
		&settlement.ArchivedStateError{}:                          "archived by the network",
		fmt.Errorf("%w: 9 XLM", settlement.ErrResourceFeeTooHigh): "more in fees",
	} {
		h.sender.chain.refuse = refusal
		if body := h.say(t, space, "deposit", "5 XLM"); !strings.Contains(body, want) {
			t.Errorf("%v:\n%s", refusal, body)
		}
	}
}

// Somebody with no wallet cannot make a call, and is told that and not handed
// a link that could never be signed.
func TestACallNeedsAWallet(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org := setUpWorkspace(t, h, space)
	if _, err := h.store.AddContractTreasury(context.Background(), org.ID, someContract(), "ops", 0); err != nil {
		t.Fatal(err)
	}
	h.sender.chain.proposals = map[uint32]*api.ChainProposal{1: open(1, "GSOMEBODY", 0, 0)}
	to := keypair.MustRandom().Address()

	for command, args := range map[string]string{
		"pay": "5 to " + to, "approve": "1", "reject": "1", "cancel": "1",
		"allow": to, "spend": "5 to " + to, "deposit": "5", "stop": "1",
	} {
		body := h.say(t, space, command, args)
		if !strings.Contains(body, "You need a wallet here first") {
			t.Errorf("/%s without a wallet:\n%s", command, body)
		}
	}
	if h.sender.chain.calls != 0 {
		t.Errorf("%d call(s) were built for somebody with no wallet", h.sender.chain.calls)
	}
}

// /approve and /reject are a vote, each way, on the proposal the contract
// numbers.
func TestApproveAndRejectAreVotes(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	heldByContract(t, h, space)
	h.sender.chain.proposals[3] = open(3, "GSOMEBODY", 1, 0)
	h.sender.chain.proposals[3].Note = "March invoice"

	body := h.say(t, space, "approve", "3")
	mentions(t, body, "/call#", "Your vote for proposal 3", "pay 250.00 to GPAYEE",
		"March invoice", "weight 1 for, 0 against")
	body = h.say(t, space, "reject", "#3")
	mentions(t, body, "/call#", "Your vote against proposal 3")
	if fmt.Sprint(h.sender.chain.votes) != "[3:true 3:false]" {
		t.Errorf("votes built: %v", h.sender.chain.votes)
	}

	for args, want := range map[string]string{
		"":   "Which proposal?",
		"9":  "no proposal with that number",
		"x7": "Which proposal?",
	} {
		if body := h.say(t, space, "approve", args); !strings.Contains(body, want) {
			t.Errorf("/approve %s:\n%s", args, body)
		}
	}

	// One whose outcome is settled takes no more votes, whatever the clock
	// says about its window.
	h.sender.chain.proposals[4] = open(4, "GSOMEBODY", 2, 0)
	h.sender.chain.proposals[4].Settles = treasuryabi.Passed
	body = h.say(t, space, "approve", "4")
	mentions(t, body, "not taking votes any more", "it passes")
	if strings.Contains(body, "/call#") {
		t.Errorf("a vote was offered on a settled proposal:\n%s", body)
	}

	h.sender.chain.refuse = &api.ContractRefusal{Code: treasuryabi.AlreadyVoted}
	if body := h.say(t, space, "approve", "3"); !strings.Contains(body, "already voted") {
		t.Errorf("a second vote:\n%s", body)
	}
}

// /execute does the one thing the proposal's state allows, and says why it
// did nothing when that is the answer.
func TestExecuteFollowsTheProposalsState(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	heldByContract(t, h, space)
	chain := &h.sender.chain
	soon := uint64(time.Now().Add(6 * time.Hour).Unix())
	past := uint64(time.Now().Add(-time.Minute).Unix())

	// Still being voted on: nothing is sent.
	chain.proposals[1] = open(1, "GA", 1, 0)
	mentions(t, h.say(t, space, "execute", "1"), "can't be carried out yet", "Voting is open")

	// Lost, and nobody has recorded it: recorded now, and not carried out.
	chain.proposals[2] = open(2, "GA", 0, 2)
	chain.proposals[2].Settles = treasuryabi.Rejected
	mentions(t, h.say(t, space, "execute", "2"), "is rejected", "recorded on the contract")

	// Passed, timelock still running: said, with when and what can be done.
	chain.proposals[3] = open(3, "GA", 2, 0)
	chain.proposals[3].Status, chain.proposals[3].ExecutableAt = treasuryabi.Passed, soon
	mentions(t, h.say(t, space, "execute", "3"), "timelock runs until", "/stop 3")

	// Settled in its favour and unrecorded, with the timelock over by the time
	// it is: recorded, then carried out.
	chain.proposals[4] = open(4, "GA", 2, 0)
	chain.proposals[4].Settles, chain.proposals[4].ExecutableAt = treasuryabi.Passed, past
	mentions(t, h.say(t, space, "execute", "4"), "Proposal 4 is carried out", "feedface", "ledger 77")

	// Already done.
	chain.proposals[5] = open(5, "GA", 2, 0)
	chain.proposals[5].Status = treasuryabi.Executed
	mentions(t, h.say(t, space, "execute", "5"), "has been carried out")

	if fmt.Sprint(chain.settled) != "[2 4]" || fmt.Sprint(chain.executed) != "[4]" {
		t.Errorf("settled %v and executed %v; want [2 4] and [4]", chain.settled, chain.executed)
	}

	// The ledger's clock is the one that counts.
	chain.proposals[6] = open(6, "GA", 2, 0)
	chain.proposals[6].Status, chain.proposals[6].ExecutableAt = treasuryabi.Passed, past
	chain.refuse = &api.ContractRefusal{Code: treasuryabi.NotExecutable}
	mentions(t, h.say(t, space, "execute", "6"), "a few seconds short")

	chain.refuse = &api.ContractRefusal{Code: treasuryabi.TransferFailed}
	mentions(t, h.say(t, space, "execute", "6"), "couldn't make that payment", "/deposit")
}

// There is no admin over a contract's proposals. Only the wallet that made one
// is offered a way to withdraw it.
func TestOnlyItsProposerIsOfferedAWithdrawal(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	_, _, wallet := heldByContract(t, h, space)
	h.sender.chain.proposals[1] = open(1, "GSOMEBODYELSE", 0, 0)
	h.sender.chain.proposals[2] = open(2, wallet, 0, 0)

	body := h.say(t, space, "cancel", "1")
	mentions(t, body, "only that wallet can withdraw it", "no admin", "/reject 1")
	if strings.Contains(body, "/call#") {
		t.Errorf("an admin was offered somebody else's proposal to withdraw:\n%s", body)
	}

	mentions(t, h.say(t, space, "cancel", "2"), "/call#", "To withdraw proposal 2")
	if fmt.Sprint(h.sender.chain.withdrawn) != "[2]" {
		t.Errorf("withdrawals built: %v", h.sender.chain.withdrawn)
	}

	h.sender.chain.proposals[2].Status = treasuryabi.Passed
	mentions(t, h.say(t, space, "cancel", "2"), "is not open")
}

// Changes to who decides and what may be paid are proposals like any other.
func TestTheRulesAreChangedByProposal(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org, _, wallet := heldByContract(t, h, space)
	chain := &h.sender.chain
	payee := keypair.MustRandom().Address()

	mentions(t, h.say(t, space, "allow", payee), "/call#", "allow")
	mentions(t, h.say(t, space, "disallow", payee), "/call#", "no timelock")
	mentions(t, h.say(t, space, "stop", "7"), "/call#", "stop proposal 7", "no timelock")

	// A voter by their member number here, which is turned into the address
	// they proved; the contract knows nothing else.
	bo := grantRole(t, h, org, "77", chat.RoleObserver)
	boWallet := keypair.MustRandom().Address()
	if err := h.store.SetMemberAddress(context.Background(), org.ID, bo, boWallet, store.AddressLinked); err != nil {
		t.Fatal(err)
	}
	mentions(t, h.say(t, space, "voter", fmt.Sprintf("#%d weight:2", bo)), "/call#", boWallet, "weight 2")
	mentions(t, h.say(t, space, "voter", wallet+" 0"), "/call#")
	mentions(t, h.say(t, space, "voter", "#99999"), "no member #99999")
	mentions(t, h.say(t, space, "voter", ""), "Whose vote?")

	// Only what was named changes; the rest of the policy is what it was.
	mentions(t, h.say(t, space, "rules", "timelock:2d limit:50 window:1d"), "/call#", "timelock 2d")
	mentions(t, h.say(t, space, "rules", ""), "Read from the contract just now", "quorum 50%")

	var kinds []string
	for _, a := range chain.proposed {
		kinds = append(kinds, a.Kind)
	}
	want := "[AllowRecipient DenyRecipient Revoke SetMember SetMember SetPolicy]"
	if fmt.Sprint(kinds) != want {
		t.Fatalf("proposed %v, want %s", kinds, want)
	}
	if m := chain.proposed[3].Member; m.Address != boWallet || m.Weight != 2 {
		t.Errorf("voter proposed: %+v", m)
	}
	if m := chain.proposed[4].Member; m.Address != wallet || m.Weight != 0 {
		t.Errorf("removal proposed: %+v", m)
	}
	policy := chain.proposed[5].Policy
	if policy.Timelock != 172_800 || policy.SpendWindow != 86_400 ||
		policy.SpendLimit.Int64() != int64(money.MustParse("50")) ||
		policy.QuorumBps != 5_000 || policy.VotingPeriod != 259_200 {
		t.Errorf("policy proposed: %+v", policy)
	}
}

// Spending under the allowance and depositing are each one member's call.
func TestSpendAndDepositAreTheMembersOwnCalls(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	_, _, wallet := heldByContract(t, h, space)
	to := keypair.MustRandom().Address()

	first := h.say(t, space, "spend", "20 to "+to)
	mentions(t, first, "/call#", "on your signature alone", "not a proposal")
	if strings.Contains(first, "another link") {
		t.Errorf("the only link somebody holds was said to be one of two:\n%s", first)
	}
	// A second link for the same wallet before the first is signed: both are
	// built for one sequence number, and it is said while it is still a choice.
	mentions(t, h.say(t, space, "deposit", "100 XLM"), "/call#", "from your wallet "+wallet, "into ops",
		"another link from me still waiting", "one at a time")
	mentions(t, h.say(t, space, "deposit", ""), "How much?")
	mentions(t, h.say(t, space, "spend", ""), "Try: /spend 20 to")
	mentions(t, h.say(t, space, "deposit", "5 DOGE"), "I don't know which DOGE you mean")

	if len(h.sender.chain.spent) != 1 || !strings.HasSuffix(h.sender.chain.spent[0], "to "+to) {
		t.Errorf("spent: %v", h.sender.chain.spent)
	}
	if fmt.Sprint(h.sender.chain.deposited) != "[100.0000000 XLM]" {
		t.Errorf("deposited: %v", h.sender.chain.deposited)
	}
}

// A workspace with both kinds of treasury has two things called "proposal 3",
// and is asked which.
func TestWithBothKindsOfTreasuryTheCommandAsksWhich(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org, contract, _ := heldByContract(t, h, space)
	classic := treasuryFor(t, h, org)
	h.sender.chain.proposals[3] = open(3, "GA", 0, 0)

	for _, command := range []string{"approve", "execute", "cancel"} {
		body := h.say(t, space, command, "3")
		mentions(t, body, "more than one treasury", "treasury:ops", "treasury:main")
	}
	mentions(t, h.say(t, space, "approve", "3 treasury:ops"), "/call#", "proposal 3 of ops")
	mentions(t, h.say(t, space, "approve", "3 treasury:"+contract.Address), "/call#")
	// A payment says which too, and it does not matter where in the line: the
	// name is not part of the note, and neither is the asset.
	to := keypair.MustRandom().Address()
	for _, line := range []string{
		"5 to " + to + " treasury:ops asset:XLM for the audit",
		"5 to " + to + " asset:XLM treasury:ops for the audit",
		"5 to " + to + " asset:XLM for the audit treasury:ops",
	} {
		mentions(t, h.say(t, space, "pay", line), "/call#", "ops's voters")
		if note := h.sender.chain.notes[len(h.sender.chain.notes)-1]; note != "for the audit" {
			t.Errorf("/pay %s: the note is %q", line, note)
		}
	}
	mentions(t, h.say(t, space, "pay", "5 to "+to), "more than one treasury")

	// Named, the classic one is read exactly as it was before there was a
	// second: by the number this deployment gave its proposal.
	mentions(t, h.say(t, space, "approve", "3 treasury:main"), "no proposal #3")
	mentions(t, h.say(t, space, "approve", "3 treasury:nowhere"), `don't know a treasury called "nowhere"`)

	// Commands that mean nothing for an account say so about that account.
	mentions(t, h.say(t, space, "rules", "treasury:main"), "an account its signers sign for")
	mentions(t, h.say(t, space, "deposit", "5 treasury:main"), "an account its signers sign for")
	_ = classic

	// And the list shows each apart.
	mentions(t, h.say(t, space, "proposals", ""), "ops (held by a contract)", "3 · pay 250.00 to GPAYEE")
}

// A workspace with only classic treasuries is read exactly as it always was.
func TestAClassicWorkspaceIsUntouched(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	proposerIn(t, h, space)

	mentions(t, h.say(t, space, "approve", "3"), "no proposal #3")
	mentions(t, h.say(t, space, "rules", ""), "this workspace has none", "/create-treasury")
	mentions(t, h.say(t, space, "deposit", "5"), "an ordinary account", "from any wallet")
	if h.sender.chain.calls != 0 {
		t.Errorf("a classic workspace built %d contract call(s)", h.sender.chain.calls)
	}
}

// /link-treasury with a contract's address adopts it, if the contract says
// whoever is asking has a vote in it.
func TestLinkingAContractAsksTheContract(t *testing.T) {
	h := newHarness(t, true)
	space := nextSpace()
	org := setUpWorkspace(t, h, space)
	address := someContract()

	mentions(t, h.say(t, space, "link-treasury", address), "You need a wallet here first")

	admin, _, _ := h.store.MemberByIdentity(context.Background(), org.ID, chat.Telegram, "42")
	wallet := keypair.MustRandom().Address()
	if err := h.store.SetMemberAddress(context.Background(), org.ID, admin.ID, wallet, store.AddressLinked); err != nil {
		t.Fatal(err)
	}

	h.sender.chain.refuse = &api.ContractRefusal{Code: treasuryabi.NotMember}
	mentions(t, h.say(t, space, "link-treasury", address), wallet, "has no vote in that treasury")
	h.sender.chain.refuse = api.ErrNotTreasuryCode
	mentions(t, h.say(t, space, "link-treasury", address), "isn't running the treasury code")

	h.sender.chain.refuse = nil
	mentions(t, h.say(t, space, "link-treasury", address), "Added.", address, "you have a vote in it")
	if fmt.Sprint(h.sender.chain.adopted) != "["+address+"]" {
		t.Errorf("adopted: %v", h.sender.chain.adopted)
	}
	mentions(t, h.say(t, space, "treasuries", ""), address, "held by a contract")
}

// A deployment that cannot reach a contract does not offer the commands that
// need one.
func TestWithoutSorobanTheContractCommandsAreNotOffered(t *testing.T) {
	db := store.New(testPool)
	svc, err := New(Config{
		Store: db, Sender: &fakeSender{db: db}, Admins: fakeAdmins{admin: true}, Network: "testnet",
		Unavailable: Unavailable{ContractTreasuries: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{}
	for _, c := range svc.Commands() {
		offered[c.Name] = true
	}
	for _, name := range []string{
		"create-treasury", "deposit", "reject", "stop", "allow", "disallow", "voter", "rules", "spend",
	} {
		if offered[name] {
			t.Errorf("/%s is offered on a deployment that cannot reach a contract", name)
		}
	}
	for _, name := range []string{"pay", "approve", "execute", "link-treasury"} {
		if !offered[name] {
			t.Errorf("/%s is gone, and it serves classic treasuries too", name)
		}
	}
}

func TestSharesAreReadExactly(t *testing.T) {
	for text, want := range map[string]uint32{
		"60": 6_000, "60%": 6_000, "66.67": 6_667, "0.01": 1, "100": 10_000, " 50.5% ": 5_050,
	} {
		if got, ok := share(text); !ok || got != want {
			t.Errorf("share(%q) = %d, %v; want %d", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "0", "0.00", "100.01", "101", "-5", "6e1", "66.667", "half", "."} {
		if got, ok := share(text); ok {
			t.Errorf("share(%q) = %d, and should not have been read", text, got)
		}
	}
}

func TestSpansAreReadWithAUnit(t *testing.T) {
	for text, want := range map[string]uint64{
		"30s": 30, "15m": 900, "12h": 43_200, "3d": 259_200, "0": 0, "none": 0, " 2D ": 172_800,
	} {
		if got, err := span(text); err != nil || got != want {
			t.Errorf("span(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "5", "d", "1w", "-3d", "1.5h", "three days"} {
		if got, err := span(text); err == nil {
			t.Errorf("span(%q) = %d, and should not have been read", text, got)
		}
	}
}
