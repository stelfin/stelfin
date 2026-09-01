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

// treasuryLinkLifetime bounds how long the link to the signing page is usable.
//
// Longer than a member's, because the work behind it is different: one person
// signs a wallet proof in the tab that is already open, while a treasury's
// signatures have to be collected from several people. Still shorter than the
// challenge itself, so an abandoned link stops working before the thing it
// names does.
const treasuryLinkLifetime = 15 * time.Minute

// linkTreasury starts proving that this workspace controls an account.
func (s *Service) linkTreasury(ctx context.Context, r *request) error {
	if s.challenges == nil {
		return r.reply(ctx, "Linking a treasury isn't available on this deployment yet.")
	}

	address := strings.TrimSpace(r.msg.Options["address"])
	if address == "" {
		address = strings.TrimSpace(r.msg.Args)
	}
	if address == "" {
		return r.reply(ctx,
			"Which account? Try: /link-treasury GABC…\n\n"+
				"It has to be the account that holds the money — you'll be asked to "+
				"prove it with the same signing weight a payment needs.")
	}
	if !strkey.IsValidEd25519PublicKey(address) {
		if strkey.IsValidContractAddress(address) {
			// Worth naming, because it is the direction the product is going
			// and a flat "not an address" would read as a typo.
			return r.reply(ctx,
				"That's a contract account. Proving one means asking the contract, "+
					"not collecting signatures, and stelfin can't do that yet.")
		}
		return r.reply(ctx,
			"That isn't an account I can ask you to prove. It needs to be a G… address.")
	}

	if existing, err := s.store.TreasuryByAddress(ctx, r.org.ID, address); err == nil {
		// Not an error. Re-proving after a signer change is an ordinary thing
		// to do, so say where things stand and let them go ahead.
		_ = existing
	} else if !errors.Is(err, store.ErrNoTreasury) {
		return err
	}

	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}

	challenge, err := s.sender.PrepareTreasuryLink(ctx, r.scope, identity, address)
	switch {
	case errors.Is(err, api.ErrLinkingUnavailable):
		return r.reply(ctx, "Linking a treasury isn't available on this deployment yet.")
	case errors.Is(err, api.ErrTreasuryUnprovable):
		return r.reply(ctx, fmt.Sprintf(
			"%s can't prove control by signature.\n\n"+
				"Its signers can't reach the weight the account needs to move money, "+
				"so nobody could complete the challenge — and nobody can spend from it "+
				"either. That's worth looking at on chain.", address))
	case err != nil:
		return err
	}

	link, err := r.links.IssueLinkLink(r.scope, challenge.Hash, time.Now().Add(treasuryLinkLifetime))
	if err != nil {
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"Prove this workspace controls %s.\n\n"+
			"Open this and collect signatures — it takes the same weight a payment "+
			"takes, so on an M-of-N account several people sign the same envelope. "+
			"The challenge is built so it can never be submitted, so signing it "+
			"cannot move anything:\n%s\n\n"+
			"The link expires in %d minutes. This message is only visible to you, so "+
			"pass the envelope from the page to the other signers, not this link.",
		address, link, int(treasuryLinkLifetime.Minutes())))
}

// treasuries lists what this workspace has proved control of.
func (s *Service) treasuries(ctx context.Context, r *request) error {
	list, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx,
			"No treasury is linked here yet. An admin can run /link-treasury GABC… "+
				"to prove this workspace controls one.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Treasuries for %s:\n", r.org.DisplayName)
	for _, t := range list {
		name := t.Label
		if name == "" {
			name = "unnamed"
		}
		// The threshold is reported as of the last read and said to be so. A
		// number presented as current, and stale, is worse than one that admits
		// its age — somebody would plan a payment around it.
		fmt.Fprintf(&b, "\n· %s — %s\n  %s, needs weight %d to spend (as of %s)",
			name, t.Address, t.Kind, t.Medium, t.RefreshedAt.UTC().Format("2 Jan 15:04 MST"))
	}
	return r.reply(ctx, b.String())
}

// treasuryCommands are registered alongside the rest.
func (s *Service) treasuryCommands() []*command {
	return []*command{
		{
			name:        "link-treasury",
			description: "Prove this workspace controls an account",
			options: []chat.Option{{
				Name: "address", Description: "The G… account that holds the money",
				Type: chat.OptString, Required: true,
			}},
			// Admin, because linking a treasury is what turns provisioning on
			// and what later commands will spend against. It confers no
			// authority over the money itself — that stays with the account's
			// signers, and no role here can stand in for a signature.
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.linkTreasury(ctx, r) },
		},
		{
			name:        "treasuries",
			description: "List the accounts this workspace has proved",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.treasuries(ctx, r) },
		},
	}
}
