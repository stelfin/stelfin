package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// linkLinkLifetime bounds how long a link to the signing page is usable.
//
// Shorter than the challenge it carries, so an abandoned link stops working
// before the thing it names does — the same relationship the confirm and enroll
// links have to their transactions.
const linkLinkLifetime = 4 * time.Minute

// linkCodeLifetime bounds a code that attaches a second chat account.
//
// Ten minutes: long enough to switch devices and type eight characters, short
// enough that a code left in a scrollback stops mattering quickly.
const linkCodeLifetime = 10 * time.Minute

// link starts proving control of an address.
func (s *Service) link(ctx context.Context, r *request) error {
	if s.challenges == nil {
		return r.reply(ctx, "Linking a wallet isn't available on this deployment yet.")
	}

	address := strings.TrimSpace(r.msg.Options["address"])
	if address == "" {
		address = strings.TrimSpace(r.msg.Args)
	}
	if address == "" {
		return r.reply(ctx,
			"Which address? Try: /link GABC…\n\n"+
				"It has to be one you can sign with — you'll be asked to prove it.")
	}
	if !strkey.IsValidEd25519PublicKey(address) {
		// Muxed and contract addresses land here too, and the message says why
		// rather than pretending they were typed wrongly.
		return r.reply(ctx,
			"That isn't a Stellar address I can ask you to prove. "+
				"It needs to be a G… account you hold the key for.")
	}

	// Already linked to this member: nothing to do, and saying so is more use
	// than issuing a challenge they do not need.
	if r.member.Address == address {
		return r.reply(ctx, "That address is already linked to you here.")
	}

	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}

	challenge, err := s.sender.PrepareLink(ctx, r.scope, identity, address)
	switch {
	case errors.Is(err, api.ErrLinkingUnavailable):
		return r.reply(ctx, "Linking a wallet isn't available on this deployment yet.")
	case err != nil:
		return err
	}

	link, err := r.links.IssueLinkLink(r.scope, challenge.Hash, time.Now().Add(linkLinkLifetime))
	if err != nil {
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"Prove you control %s.\n\n"+
			"Open this and sign — the challenge is built so it can never be "+
			"submitted, so signing it cannot move anything:\n%s\n\n"+
			"The link expires in %d minutes.",
		address, link, int(linkLinkLifetime.Minutes())))
}

// linkCode issues a code that attaches another chat account to this member.
func (s *Service) linkCode(ctx context.Context, r *request) error {
	// The code adds a handle to a member. If that member has proved nothing,
	// there is nothing worth attaching to and the code would be a way to
	// impersonate someone who is not yet anyone.
	if !r.member.HasAddress() {
		return r.reply(ctx,
			"Link a wallet here first with /link — a code attaches another chat "+
				"account to the member you've already proved yourself to be.")
	}

	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}

	code, err := s.store.IssueLinkCode(ctx, r.org.ID, r.member.ID, identity, linkCodeLifetime)
	if err != nil {
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"From your other chat account, in this workspace, run:\n\n/claim %s\n\n"+
			"It expires in %d minutes, and asking again replaces it.",
		code, int(linkCodeLifetime.Minutes())))
}

// claim spends a code, attaching this chat account to the member that issued it.
func (s *Service) claim(ctx context.Context, r *request) error {
	code := strings.ToUpper(strings.TrimSpace(r.msg.Options["code"]))
	if code == "" {
		code = strings.ToUpper(strings.TrimSpace(r.msg.Args))
	}
	if code == "" {
		return r.reply(ctx, "Which code? Run /link-code from your other chat account first.")
	}

	// Already speaking for a member with an address: claiming a code would
	// silently move this account to a different member, which is not something
	// a mistyped command should be able to do.
	if r.member.HasAddress() {
		return r.reply(ctx,
			"This chat account already speaks for a member with a linked wallet here.")
	}

	member, err := s.store.ClaimLinkCode(ctx, r.org.ID, code,
		r.actor.Channel, r.actor.UserID, r.actor.Handle)
	switch {
	case errors.Is(err, store.ErrNoLinkCode):
		// Unknown, expired and already-spent are one answer: which it was
		// narrows a guess, and none of the three is actionable differently.
		return r.reply(ctx, "That code isn't valid. Ask for a new one with /link-code.")
	case err != nil:
		return err
	}

	linked, err := s.store.Member(ctx, r.org.ID, member)
	if err != nil {
		return err
	}
	return r.reply(ctx, fmt.Sprintf(
		"Done — this account now speaks for the same member as your other one.\n\n"+
			"Wallet: %s", linked.Address))
}

// linkCommands are registered alongside the rest.
func (s *Service) linkCommands() []*command {
	return []*command{
		{
			name:        "link",
			description: "Prove you control a Stellar address",
			options: []chat.Option{{
				Name: "address", Description: "The G… address to link",
				Type: chat.OptString, Required: true,
			}},
			minRole: chat.RoleNone,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.link(ctx, r) },
		},
		{
			name:        "link-code",
			description: "Get a code to attach another chat account to you",
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.linkCode(ctx, r) },
		},
		{
			name:        "claim",
			description: "Attach this chat account to a member, using a code",
			options: []chat.Option{{
				Name: "code", Description: "The code from /link-code",
				Type: chat.OptString, Required: true,
			}},
			minRole: chat.RoleNone,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.claim(ctx, r) },
		},
	}
}
