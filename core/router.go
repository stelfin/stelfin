package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// request is everything a command handler needs, resolved once.
type request struct {
	msg   chat.Inbound
	org   store.Org
	actor chat.Actor
	// member is the person speaking, created the first time they are seen.
	member store.Member
	// role is what they may ask the bot to do here. Never what they may spend.
	role  chat.Role
	scope api.Scope
	out   api.Replier
	links api.Linker
}

// reply sends one message back to where the request came from.
//
// Every reply is ephemeral. Some carry a link that authorises money and the
// rest quote someone's own instruction back at them; neither belongs in a room
// several hundred people can read. Unconditional, so a command added later
// cannot leak by forgetting the flag.
func (r *request) reply(ctx context.Context, body string) error {
	return r.out.Send(ctx, r.msg.Conversation, r.actor, chat.Reply{Text: body, Ephemeral: true})
}

// Handle processes one inbound message.
//
// It returns nil for messages it deliberately declines to act on — an
// unregistered space, a duplicate delivery — because the caller's job is to
// keep the webhook healthy rather than to surface every non-event as a failure.
func (s *Service) Handle(ctx context.Context, m chat.Inbound, out api.Replier, links api.Linker) error {
	if m.DedupeID == "" {
		return errors.New("core: inbound message has no dedupe id")
	}

	// Tenancy first, before a single byte of the content is interpreted.
	org, registered, err := s.store.OrgForSpace(ctx, m.Conversation.Channel, m.Conversation.SpaceID)
	if err != nil {
		return err
	}

	// An unregistered space is where setup has to be possible, and it is the
	// only thing that can happen there: the bot has been added somewhere with
	// no tenant behind it, so there is nobody to be a member of and no books to
	// read. Everything else is silence — answering would be talking to a room
	// that never asked.
	if !registered {
		if m.Command != commandSetup && m.Command != commandConnect {
			return nil
		}
		claimed, err := s.claimDelivery(ctx, m)
		if err != nil || !claimed {
			return err
		}
		if m.Command == commandConnect {
			return s.connectHere(ctx, &request{msg: m, actor: m.Actor, out: out})
		}
		return s.runSetup(ctx, m, out)
	}

	claimed, err := s.claimDelivery(ctx, m)
	if err != nil || !claimed {
		return err
	}

	// The transport identity becomes the domain identity here and nowhere else.
	// Actor.Ref is channel-scoped, so the same numeric id on two platforms stays
	// two different owners.
	member, err := s.store.EnsureMember(ctx, org.ID,
		m.Actor.Channel, m.Actor.UserID, m.Actor.Handle)
	if err != nil {
		return err
	}
	role, err := s.store.EffectiveRole(ctx, org.ID, member.ID, m.Actor.Channel, m.Actor.Roles)
	if err != nil {
		return err
	}

	req := &request{
		msg: m, org: org, actor: m.Actor, member: member, role: role,
		scope: api.Scope{Org: org.ID, OwnerRef: m.Actor.Ref()},
		out:   out, links: links,
	}

	if !org.Active() {
		// Suspended tenants keep their history readable and move no money.
		return req.reply(ctx, "This workspace is suspended. Nothing can be sent right now.")
	}

	if m.Command == "" {
		return s.sender.HandleSend(ctx, req.scope, m, out, links)
	}

	cmd, known := s.commands[m.Command]
	if !known {
		return req.reply(ctx, fmt.Sprintf(
			"I don't know /%s. Try /help.", m.Command))
	}
	if !role.AtLeast(cmd.minRole) {
		// Deliberately says what is needed rather than what they have: the
		// useful information is who to ask.
		return req.reply(ctx, fmt.Sprintf(
			"/%s needs the %s role in this workspace.", cmd.name, cmd.minRole))
	}
	if err := cmd.run(ctx, s, req); err != nil {
		if errors.Is(err, api.ErrReadOnly) {
			// Not a failure, and not worth an error in the log every time
			// somebody tries: the deployment is doing what it was told to.
			return req.reply(ctx, readOnlyReply)
		}
		// Still returned, so it is logged with everything in it. But the person
		// who typed the command is told something too: an error that reaches
		// only the log looks, from the chat, exactly like a bot that is not
		// there. Nothing of the error itself goes in the reply — it can carry
		// addresses, SQL and upstream URLs.
		if replyErr := req.reply(ctx, commandFailedReply); replyErr != nil {
			return fmt.Errorf("%w (and the reply saying so failed: %v)", err, replyErr)
		}
		return err
	}
	return nil
}

// readOnlyReply answers anything that would build or submit while the
// deployment's kill switch is on.
const readOnlyReply = "stelfin is in read-only mode right now, so I can't start or submit anything. " +
	"Nothing is lost — proposals and their signatures are kept, and you can still look. " +
	"Whoever runs this deployment turned it on, and will turn it off."

// commandFailedReply is all a member is told when a command fails for a reason
// that is not theirs. It promises only what is true of every command here:
// anything that moves money needs a signature that was never collected.
const commandFailedReply = "Something went wrong on my side, so I stopped. " +
	"Nothing was sent or changed — please try that again in a moment."

// claimDelivery records the delivery, reporting whether this caller won it.
//
// Platforms retry anything they consider slow or failed. Claiming before
// dispatch means one instruction cannot become two confirmations whatever that
// instruction turns out to be — and a duplicate gets silence, because replying
// again would tell the same person twice.
func (s *Service) claimDelivery(ctx context.Context, m chat.Inbound) (bool, error) {
	return s.store.ClaimMessage(ctx, m.DedupeID, m.Actor.Ref())
}
