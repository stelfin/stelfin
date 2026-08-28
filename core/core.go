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

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ledger/store"
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
	Challenges() *identity.Challenges
}

// Config wires the router.
type Config struct {
	Store  *store.Store
	Sender Sender
	Admins Admins
	// Network is the network new orgs are created on, matching the platform
	// org. A tenant is bound to one for its lifetime.
	Network string
	Logger  *slog.Logger
}

// Service routes inbound messages.
type Service struct {
	store      *store.Store
	sender     Sender
	admins     Admins
	challenges *identity.Challenges
	network    string
	log        *slog.Logger
	commands   map[string]*command
	catalog    []chat.Command
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
		store:      cfg.Store,
		sender:     cfg.Sender,
		admins:     cfg.Admins,
		challenges: cfg.Sender.Challenges(),
		network:    cfg.Network,
		log:        log,
	}
	s.registerCommands()
	return s, nil
}

// Commands returns the catalog to publish to each platform.
func (s *Service) Commands() []chat.Command { return s.catalog }
