package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
)

// reclaimLinkLifetime bounds the link that hands an account back.
//
// Shorter than the envelope it carries, so an abandoned link stops working
// before the thing it names does — the same relationship every other link here
// has to its transaction.
const reclaimLinkLifetime = 15 * time.Minute

// reclaim hands a provisioned account back to the operator.
//
// The other half of the enrolment limiter. Provisioning is capped because every
// account costs the operator reserve permanently; a cap with no way back is
// still a leak, just a slower one.
func (s *Service) reclaim(ctx context.Context, r *request) error {
	if !r.member.HasAddress() {
		return r.reply(ctx, "You don't have an account here to hand back.")
	}

	reclaim, err := s.sender.PrepareReclaim(ctx, r.scope, r.member.Address)
	switch {
	case errors.Is(err, api.ErrNothingToReclaim):
		// Their own wallet is theirs. Building them a transaction that deletes
		// it would be doing something nobody asked for.
		return r.reply(ctx,
			"That wallet wasn't provisioned by stelfin, so there's nothing to hand back. "+
				"It's yours — if you want to close it, do that from your own wallet.")
	case errors.Is(err, api.ErrAccountNotEmpty):
		return r.reply(ctx, fmt.Sprintf(
			"Not yet — %v.\n\nHanding the account back sweeps its XLM and deletes it, "+
				"and a trustline can only be removed once it's empty. Send the balance "+
				"somewhere first, then run /reclaim again.", err))
	case errors.Is(err, api.ErrCannotReclaim):
		return r.reply(ctx, fmt.Sprintf("I can't build that hand-back: %v", err))
	case err != nil:
		return err
	}

	link, err := r.links.IssueReclaimLink(r.scope, reclaim.Hash, time.Now().Add(reclaimLinkLifetime))
	if err != nil {
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"This hands %s back and releases %s XLM of reserve to the operator.\n\n"+
			"It deletes the account: its remaining XLM goes to %s and the address "+
			"stops existing. There is no undo, so read the page before you sign.\n%s\n\n"+
			"The link expires in %d minutes.",
		reclaim.Address, reclaim.Releasing, reclaim.Destination, link,
		int(reclaimLinkLifetime.Minutes())))
}

// reclaimCommands are registered alongside the rest.
func (s *Service) reclaimCommands() []*command {
	return []*command{
		{
			name:        "reclaim",
			description: "Hand back the wallet stelfin provisioned for you",
			// Anyone with a provisioned account, which is checked from the
			// grant rather than from a role: the operator paid for it, and the
			// person holding it should not need permission to give it back.
			minRole: chat.RoleNone,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.reclaim(ctx, r) },
		},
	}
}
