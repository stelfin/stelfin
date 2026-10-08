package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// read.
	if !registered {
		return s.handleUnregistered(ctx, m, out)
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
		if s.unavailable.FreeText {
			// Said plainly, and once per message addressed to the bot. The
			// alternative was handing the sentence to a decoder with no model
			// behind it and apologising for the failure, every time, as if
			// trying again might help.
			return req.reply(ctx, noModelReply)
		}
		return s.sender.HandleSend(ctx, req.scope, m, out, links)
	}

	name := m.Command
	if name == commandStart {
		// What Telegram sends when somebody opens a chat with a bot, and what
		// people type at one they have not met. Here it means the same as
		// asking what there is.
		name = commandHelp
	}
	cmd, known := s.commands[name]
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

// handleUnregistered answers a message from a space no workspace has claimed.
//
// Setup and connect run here, because here is the only place they can. For
// everything else the rule used to be silence, on the reasoning that answering
// is talking to a room that never asked. That is still right for a room. It was
// wrong for a person: someone who types a command at the bot has asked, and
// silence told them the bot was broken. It was most wrong in a direct message,
// which is where the bot itself sends people — "open a chat with me, press
// Start" — and where it then ignored the Start it had asked for.
//
// So the line is drawn at who is being answered. A command gets a private
// reply saying what has to happen first. A direct message gets an introduction.
// Ordinary conversation in a group gets nothing, and costs nothing: it is not
// claimed, so it leaves no row behind.
func (s *Service) handleUnregistered(ctx context.Context, m chat.Inbound, out api.Replier) error {
	r := &request{msg: m, actor: m.Actor, out: out}

	switch {
	case m.Command == commandSetup, m.Command == commandConnect:
	case m.Conversation.IsDM:
	case m.Command == commandStart:
	case m.Command != "" && s.commands[m.Command] != nil:
		// A command this bot has. One it has never heard of was meant for some
		// other bot in the same group, and is none of its business.
	default:
		return nil
	}

	claimed, err := s.claimDelivery(ctx, m)
	if err != nil || !claimed {
		return err
	}

	switch {
	case m.Command == commandConnect:
		return s.connectHere(ctx, r)
	case m.Command == commandSetup:
		return s.runSetup(ctx, m, out)
	case m.Conversation.IsDM:
		return r.reply(ctx, s.introduction())
	default:
		return r.reply(ctx, notSetUpReply)
	}
}

// notSetUpReply answers a command in a group or server with no workspace.
//
// A constant, and the same wherever it is said: a space that has joined nothing
// must learn nothing from asking, including whether a workspace by some name
// exists.
const notSetUpReply = "This space isn't set up with stelfin yet. " +
	"One of its administrators can run /setup <name> here — for example /setup Acme DAO — " +
	"or /connect <code> to join a workspace that already exists. " +
	"After that, /help lists what I can do."

// introduction is what somebody is told in a direct message.
//
// It has to say where the work happens, because nothing else does: every
// command acts on a workspace, a workspace is a group or a server, and a person
// who has just pressed Start has no way to know that this chat is only where
// their private links arrive.
func (s *Service) introduction() string {
	var b strings.Builder
	b.WriteString("I'm stelfin. A DAO runs its Stellar treasury through me from its group chat: " +
		"someone proposes a payment, the treasury's signers approve it on their own devices, " +
		"and it settles on Stellar. I never hold anyone's key.\n\n" +
		"I work in a group or a server, not here. This chat is where I send you your own " +
		"signing links, privately, so it's worth keeping open.\n\n" +
		"To start: add me to your group, then have one of its administrators run " +
		"/setup <name> there. After that, /help in the group lists what I can do.")
	if s.network == "testnet" {
		b.WriteString("\n\nThis deployment runs on Stellar testnet. No real money moves.")
	}
	return b.String()
}

// noModelReply answers a sentence on a deployment with no model to read it.
const noModelReply = "I can't read a sentence here: this deployment has no language model connected, " +
	"so I only understand commands. /help lists them — /pay proposes a payment from the treasury."

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
