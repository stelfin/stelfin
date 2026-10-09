// Package core routes an inbound chat message to whatever should happen next.
//
// It is the one place that answers, in order, the questions every message needs
// answered before anything else can look at it:
//
//	which tenant is this?        the space it arrived in, and nothing else
//	have we already handled it?  claimed exactly once, by delivery id
//	who is speaking?             a member of that org, created on first sight
//	what may they do?            their role there, which is not their platform's
//	what did they ask for?       a command, or free text for the decoder
//
// The order matters. Tenancy is resolved before a single byte of content is
// interpreted, so a message from a space no org has claimed costs nothing and
// gets no reply. The claim happens before dispatch, so a platform's retry
// cannot run one instruction twice whatever that instruction turns out to be.
//
// What this package deliberately does not decide is whether a payment is
// allowed. A role here decides who the bot will talk to. Whether an approval
// counts is settled by the network, against the treasury account's own signer
// list, and nothing in this process can influence that.
package core

import (
	"context"
	"errors"
	"log/slog"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/contracts/treasuryabi"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Admins answers whether someone administers the space a message arrived in.
// *chat.Registry implements it.
type Admins interface {
	IsSpaceAdmin(ctx context.Context, to chat.Conversation, actor chat.Actor) (bool, error)
}

// Sender runs the free-text payment path and issues address challenges.
// *api.Service implements it.
type Sender interface {
	HandleSend(ctx context.Context, scope api.Scope, m chat.Inbound, out api.Replier, links api.Linker) error
	PrepareLink(ctx context.Context, scope api.Scope, identity store.IdentityID, address string) (*api.LinkChallenge, error)
	PrepareTreasuryLink(ctx context.Context, scope api.Scope, identity store.IdentityID, address string) (*api.LinkChallenge, error)
	ProposePayment(ctx context.Context, scope api.Scope, p api.ProposePaymentParams) (*api.ProposalView, error)
	LoadProposal(ctx context.Context, scope api.Scope, id store.ProposalID) (*api.ProposalView, error)
	ExecuteProposal(ctx context.Context, scope api.Scope, id store.ProposalID, by store.IdentityID) (*settlement.Result, error)
	CancelProposal(ctx context.Context, scope api.Scope, id store.ProposalID, by store.MemberID, admin bool) (store.Proposal, error)
	Balances(ctx context.Context, scope api.Scope, treasuries bool, linked string) (*api.BalanceReport, error)
	ProposeTrust(ctx context.Context, scope api.Scope, treasury store.TreasuryID, asset string, by store.MemberID) (*api.ProposalView, string, error)
	ProposeUntrust(ctx context.Context, scope api.Scope, treasury store.TreasuryID, asset string, by store.MemberID) (*api.ProposalView, string, error)
	QuoteSwap(ctx context.Context, scope api.Scope, treasury store.TreasuryID, amount money.Stroops, from, to string) (*api.SwapQuote, error)
	ProposeSwap(ctx context.Context, scope api.Scope, p api.SwapParams) (*api.ProposalView, *api.SwapQuote, money.Stroops, error)
	PrepareReclaim(ctx context.Context, scope api.Scope, address string) (*api.Reclaim, error)
	RegisterConnector(ctx context.Context, scope api.Scope, by store.MemberID, name, kind string) (store.Connector, error)
	PrepareGrant(ctx context.Context, scope api.Scope, identity store.IdentityID, by store.MemberID,
		name string, treasury store.TreasuryID, perHour int) (*api.GrantRequest, error)
	ReadConnector(ctx context.Context, scope api.Scope, name, selector string) (connector.Observation, store.Connector, error)
	DraftPayroll(ctx context.Context, scope api.Scope, by store.MemberID, name, selector, memo string) (store.Draft, error)
	ProposeDraft(ctx context.Context, scope api.Scope, id store.DraftID, treasury store.TreasuryID,
		by store.MemberID) (*api.ProposalView, store.Draft, error)
	SheetsAccount() string
	AllowContract(ctx context.Context, scope api.Scope, by store.MemberID, address, label string) (store.AllowedContract, error)
	ProposeInvoke(ctx context.Context, scope api.Scope, treasury store.TreasuryID,
		label, function, arguments string, by store.MemberID) (*api.ProposalView, error)
	ProposeRestore(ctx context.Context, scope api.Scope, treasury store.TreasuryID,
		label, function, arguments string, by store.MemberID) (*api.ProposalView, error)
	Challenges() *identity.Challenges

	// A treasury held by a contract. See api/daotreasury.go.
	CreateContractTreasury(ctx context.Context, scope api.Scope, p api.CreateTreasuryParams) (*api.TreasuryState, error)
	AdoptContractTreasury(ctx context.Context, scope api.Scope, address, label string,
		member store.Member, by store.IdentityID) (*api.TreasuryState, error)
	ContractTreasuryState(ctx context.Context, scope api.Scope, treasury store.Treasury) (*api.TreasuryState, error)
	ChainProposals(ctx context.Context, scope api.Scope, treasury store.Treasury, limit int) ([]api.ChainProposal, error)
	ChainProposalByID(ctx context.Context, scope api.Scope, treasury store.Treasury, id uint32) (*api.ChainProposal, error)
	ProposeOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, member store.Member,
		action treasuryabi.Action, note string) (*api.PreparedCall, error)
	VoteOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, member store.Member,
		proposal *api.ChainProposal, approve bool) (*api.PreparedCall, error)
	WithdrawOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, member store.Member,
		proposal *api.ChainProposal) (*api.PreparedCall, error)
	SpendOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, member store.Member,
		asset txnbuild.Asset, to string, amount money.Stroops) (*api.PreparedCall, error)
	DepositOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, member store.Member,
		asset txnbuild.Asset, amount money.Stroops) (*api.PreparedCall, error)
	ExecuteOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, id uint32) (settlement.Result, error)
	SettleOnChain(ctx context.Context, scope api.Scope, treasury store.Treasury, id uint32) (string, error)
	PayAction(asset txnbuild.Asset, to string, amount money.Stroops) (treasuryabi.Action, error)
	TreasuryAsset(text string) (txnbuild.Asset, error)
	DescribeAction(a treasuryabi.Action) string
	WaitingCalls(ctx context.Context, scope api.Scope) (int, error)
}

// Config wires the router.
type Config struct {
	Store  *store.Store
	Sender Sender
	Admins Admins
	// Network is the network new orgs are created on, matching the platform
	// org. A tenant is bound to one for its lifetime.
	Network string
	// Unavailable names what this deployment has not been given. The zero
	// value means it has everything.
	Unavailable Unavailable
	Logger      *slog.Logger
}

// Unavailable is what a deployment cannot do, for want of configuration.
//
// Stated as what is missing rather than what is present because the difference
// decides what a mistake looks like. Forgetting to say a capability is there
// would hide working commands; forgetting to say it is missing leaves a command
// in the menu that answers "not on this deployment". The second is the bug this
// exists to remove, and it should not also be what happens by default — but a
// service wired with no opinion at all should offer everything, as it did
// before this type existed.
type Unavailable struct {
	// Model: no language model is connected. A sentence is still read, by a
	// fixed grammar, in the one form "send <amount> to <who>" — so this hides
	// nothing, and only changes what /help says a sentence can be.
	Model bool
	// Connectors: neither a connector key nor a Google service account.
	Connectors bool
	// ContractCalls: not switched on, or no Soroban RPC to make them through.
	ContractCalls bool
	// ContractTreasuries: no Soroban RPC, or no treasury code uploaded to the
	// network this deployment runs on.
	ContractTreasuries bool
}

// lacks reports whether any capability in needs is missing.
func (u Unavailable) lacks(needs capability) bool {
	return needs&needsConnectors != 0 && u.Connectors ||
		needs&needsContractCalls != 0 && u.ContractCalls ||
		needs&needsSoroban != 0 && u.ContractTreasuries
}

// Service routes inbound messages.
type Service struct {
	store       *store.Store
	sender      Sender
	admins      Admins
	challenges  *identity.Challenges
	network     string
	unavailable Unavailable
	log         *slog.Logger
	commands    map[string]*command
	catalog     []chat.Command
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("core: store is required")
	case cfg.Sender == nil:
		return nil, errors.New("core: sender is required")
	case cfg.Admins == nil:
		return nil, errors.New("core: admin checker is required")
	case cfg.Network != "testnet" && cfg.Network != "public":
		return nil, errors.New("core: network must be testnet or public")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	s := &Service{
		store:       cfg.Store,
		sender:      cfg.Sender,
		admins:      cfg.Admins,
		challenges:  cfg.Sender.Challenges(),
		network:     cfg.Network,
		unavailable: cfg.Unavailable,
		log:         log,
	}
	s.registerCommands()
	return s, nil
}

// Commands returns the catalog to publish to each platform.
func (s *Service) Commands() []chat.Command { return s.catalog }
