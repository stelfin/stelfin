package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// commandConnect is the one command besides setup that can run in a space with
// no workspace behind it.
const commandConnect = "connect"

// spaceCodeLifetime bounds a code that attaches another chat space.
//
// Long enough to add the bot to the other platform and find the command there;
// short enough that a code left in a scrollback stops mattering the same day.
const spaceCodeLifetime = 15 * time.Minute

// connect attaches another chat space to this workspace.
//
// One command, two halves, because the authority has two halves. In a space
// that is already a workspace, an admin of it asks for a code. In a space that
// is not, an administrator of that space presents one. Neither half is enough:
// an admin here cannot pull somebody else's server in, and whoever runs a
// server cannot attach it to a treasury they have no standing in.
func (s *Service) connect(ctx context.Context, r *request) error {
	if code := argument(r.msg, "code"); code != "" {
		return r.reply(ctx,
			"This space is already part of a workspace, so there's nothing to connect it to. "+
				"Run /connect with no code to get one for your other space.")
	}
	if r.role < chat.RoleAdmin {
		return r.reply(ctx, "/connect needs the admin role in this workspace.")
	}

	code, err := s.store.IssueSpaceCode(ctx, r.org.ID, r.member.ID, spaceCodeLifetime)
	if err != nil {
		return err
	}
	s.log.Info("space code issued", "org", r.org.ID, "by", r.member.ID)
	return r.reply(ctx, fmt.Sprintf(
		"Add me to your other server or group, and have an administrator there run:\n\n"+
			"/connect %s\n\n"+
			"That space then shares this workspace: the same treasury, proposals and "+
			"members. The code works once, expires in %d minutes, and asking again "+
			"replaces it. Anyone who has it can attach a space they administer, so "+
			"don't post it anywhere public.",
		code, int(spaceCodeLifetime.Minutes())))
}

// connectHere handles /connect in a space that is not yet a workspace.
func (s *Service) connectHere(ctx context.Context, r *request) error {
	admin, err := s.admins.IsSpaceAdmin(ctx, r.msg.Conversation, r.actor)
	if err != nil {
		s.log.Warn("could not establish space admin", "error", err)
		return r.reply(ctx, "I couldn't check whether you administer this space. Try again in a moment.")
	}
	if !admin {
		return r.reply(ctx, "Only an administrator of this space can connect it to a workspace.")
	}
	if r.msg.Conversation.IsDM {
		return r.reply(ctx, "Run /connect in the group or server you want to add, not in a direct message.")
	}

	code := argument(r.msg, "code")
	if code == "" {
		return r.reply(ctx,
			"Which workspace? Run /connect in a space that's already set up to get a "+
				"code, then /connect <code> here. To start a new workspace instead: /setup <name>.")
	}

	org, err := s.store.ClaimSpaceCode(ctx, code,
		r.msg.Conversation.Channel, r.msg.Conversation.SpaceID, r.actor.Ref())
	switch {
	case errors.Is(err, store.ErrNoSpaceCode):
		return r.reply(ctx, "That code isn't valid. Ask for a new one with /connect in the other space.")
	case errors.Is(err, store.ErrSpaceRegistered):
		return r.reply(ctx, "This space was just set up by someone else. Run /whoami to see where it stands.")
	case err != nil:
		return err
	}

	s.log.Info("space connected", "org", org.ID,
		"channel", r.msg.Conversation.Channel, "space", r.msg.Conversation.SpaceID, "by", r.actor.Ref())
	return r.reply(ctx, fmt.Sprintf(
		"This space is now part of %q.\n\n"+
			"You're new to me here, so you have no role yet — run /whoami for your member "+
			"number. If you're already a member from the other space, run /link-code "+
			"there and /claim here, and I'll know you're the same person.",
		org.DisplayName))
}

// argument reads a command's single argument from wherever the platform put
// it: the named option on Discord, the text on Telegram.
func argument(m chat.Inbound, name string) string {
	if v := strings.TrimSpace(m.Options[name]); v != "" {
		return v
	}
	return strings.TrimSpace(m.Args)
}

// ---- Saved recipients.

// save records an address under a name, for the person saying it.
//
// The name is what a later sentence refers to — "send 50 to mum" — so it is
// private to whoever saved it. A shared address book would let one member
// decide where another member's sentence goes.
func (s *Service) save(ctx context.Context, r *request) error {
	name, address := strings.TrimSpace(r.msg.Options["name"]), strings.TrimSpace(r.msg.Options["address"])
	if name == "" && address == "" {
		// Text form: the address is the last word, and everything before it is
		// the name, so a name can have spaces in it.
		words := strings.Fields(r.msg.Args)
		if len(words) >= 2 {
			address = words[len(words)-1]
			name = strings.Join(words[:len(words)-1], " ")
		}
	}
	if name == "" || address == "" {
		return r.reply(ctx, "Try: /save mum GABC…\n\nThen \"send 50 to mum\" means that address — for you, and only you.")
	}
	if !strkey.IsValidEd25519PublicKey(address) {
		return r.reply(ctx, "That isn't a Stellar account. It needs to be a G… address.")
	}
	if strkey.IsValidEd25519PublicKey(name) {
		// A name that is itself an address would make "send to G…" mean two
		// different accounts depending on which lookup ran first.
		return r.reply(ctx, "A name can't itself be an address.")
	}
	if len([]rune(name)) > 40 {
		return r.reply(ctx, "That name is too long. Forty characters or fewer, please.")
	}

	err := s.store.SaveBeneficiary(ctx, r.org.ID, r.actor.Ref(), name, address)
	switch {
	case errors.Is(err, store.ErrBeneficiaryExists):
		return r.reply(ctx, fmt.Sprintf(
			"You already have someone saved as %q. /forget %s first if you mean to replace them — "+
				"I won't quietly repoint a name at a different address.", name, name))
	case err != nil:
		return err
	}
	return r.reply(ctx, fmt.Sprintf("Saved. For you, %q now means %s.", name, address))
}

// saved lists the speaker's saved recipients.
func (s *Service) saved(ctx context.Context, r *request) error {
	list, err := s.store.Beneficiaries(ctx, r.org.ID, r.actor.Ref())
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "You haven't saved anyone yet. /save mum GABC… saves one.")
	}
	var b strings.Builder
	b.WriteString("Your saved recipients:\n")
	for _, x := range list {
		fmt.Fprintf(&b, "\n· %s — %s", x.Label, x.Address)
	}
	return r.reply(ctx, b.String())
}

// forget removes one of the speaker's saved recipients.
func (s *Service) forget(ctx context.Context, r *request) error {
	name := argument(r.msg, "name")
	if name == "" {
		return r.reply(ctx, "Forget whom? Try: /forget mum")
	}
	err := s.store.ForgetBeneficiary(ctx, r.org.ID, r.actor.Ref(), name)
	switch {
	case errors.Is(err, store.ErrNoBeneficiary):
		return r.reply(ctx, fmt.Sprintf("You don't have anyone saved as %q. /saved lists who you do.", name))
	case err != nil:
		return err
	}
	return r.reply(ctx, fmt.Sprintf("Forgotten. %q no longer means anything to me.", name))
}

// ---- Spend limits.

// limits shows or sets the ceilings this workspace holds its own proposals to.
//
// A classic multisig has no contract to enforce a limit: whatever its signers
// sign, the network executes. These are checked before an envelope is built, so
// a proposal over one never reaches a signer. They are a guard against mistakes
// — an extra zero, a pasted column — and not against the signers themselves,
// who can always sign something built elsewhere.
func (s *Service) limits(ctx context.Context, r *request) error {
	perPayment, perDay := strings.TrimSpace(r.msg.Options["payment"]), strings.TrimSpace(r.msg.Options["daily"])
	if perPayment == "" && perDay == "" {
		// Text form: pairs of a word and a value, in either order.
		words := strings.Fields(strings.ToLower(r.msg.Args))
		if len(words)%2 != 0 {
			return r.reply(ctx, limitsUsage)
		}
		for i := 0; i < len(words); i += 2 {
			switch words[i] {
			case "payment":
				perPayment = words[i+1]
			case "daily":
				perDay = words[i+1]
			default:
				return r.reply(ctx, limitsUsage)
			}
		}
	}

	policy, err := s.store.SpendPolicyOf(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if perPayment == "" && perDay == "" {
		return r.reply(ctx, describeLimits(policy)+"\n\n"+limitsUsage)
	}

	if r.role < chat.RoleAdmin {
		return r.reply(ctx, "Changing limits needs the admin role in this workspace.")
	}
	for _, change := range []struct {
		value string
		into  *money.Stroops
	}{{perPayment, &policy.PerPayment}, {perDay, &policy.Daily}} {
		switch value := strings.ToLower(change.value); value {
		case "":
		case "none", "off":
			*change.into = 0
		default:
			n, err := money.Parse(value)
			if err != nil || n <= 0 {
				return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount. A number, or \"none\".", change.value))
			}
			*change.into = n
		}
	}
	if policy.PerPayment > 0 && policy.Daily > 0 && policy.PerPayment > policy.Daily {
		return r.reply(ctx,
			"The per-payment limit is larger than the daily one, so it could never be reached. "+
				"Set a daily limit at least as large.")
	}

	if err := s.store.SetSpendPolicy(ctx, r.org.ID, policy); err != nil {
		return err
	}
	s.log.Info("spend limits changed", "org", r.org.ID,
		"per_payment", policy.PerPayment.String(), "daily", policy.Daily.String(), "by", r.member.ID)
	return r.reply(ctx, describeLimits(policy))
}

const limitsUsage = "Try: /limits payment 5000 daily 20000 — or \"none\" in place of a number to lift one."

func describeLimits(p store.SpendPolicy) string {
	show := func(n money.Stroops) string {
		if n == 0 {
			return "none"
		}
		return n.Display()
	}
	return fmt.Sprintf(
		"Limits on what I'll put to this treasury's signers:\n\n"+
			"· one proposal: %s\n· proposed in any 24 hours: %s\n\n"+
			"The deployment has ceilings of its own above these.",
		show(p.PerPayment), show(p.Daily))
}

// ---- Tokens for agents.

// agentToken mints a token an outside agent reads this workspace with, over MCP.
//
// The token is shown once, here, and only its hash is kept. It can read
// balances, treasuries, history and proposals for this workspace and nothing
// else; there is no token that can sign or submit.
func (s *Service) agentToken(ctx context.Context, r *request) error {
	label := strings.TrimSpace(r.msg.Options["label"])
	tier := strings.ToLower(strings.TrimSpace(r.msg.Options["tier"]))
	if label == "" {
		// Text form: the label, then optionally the tier as the last word.
		words := strings.Fields(r.msg.Args)
		if n := len(words); n > 1 && (strings.EqualFold(words[n-1], store.TierPropose) || strings.EqualFold(words[n-1], store.TierRead)) {
			tier, words = strings.ToLower(words[n-1]), words[:n-1]
		}
		label = strings.Join(words, " ")
	}
	if label == "" {
		return r.reply(ctx,
			"What is it for? Try: /agent-token reporting\n\n"+
				"The label is how you'll recognise it in /agent-tokens when you want to revoke it, "+
				"and how a draft the agent writes is marked. Add \"propose\" at the end to let it "+
				"draft payrolls for a member to review: /agent-token payroll-bot propose")
	}
	if tier == "" {
		tier = store.TierRead
	}
	if tier != store.TierRead && tier != store.TierPropose {
		return r.reply(ctx, "A token is read or propose. Try: /agent-token payroll-bot propose")
	}
	if len([]rune(label)) > 40 {
		return r.reply(ctx, "That label is too long. Forty characters or fewer, please.")
	}

	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}
	token, record, err := s.store.IssueMCPToken(ctx, r.org.ID, label, tier, identity, nil)
	if err != nil {
		return err
	}
	s.log.Info("agent token issued", "org", r.org.ID, "token", record.ID, "by", r.member.ID)
	can := "An agent presenting it can read this workspace's balances, treasuries, history " +
		"and proposals. It cannot propose, sign or send anything."
	if tier == store.TierPropose {
		can = "An agent presenting it can read this workspace's balances, treasuries, history " +
			"and proposals, and write draft payrolls marked as its own. A draft is not a " +
			"proposal: a member has to propose it and the treasury's signers have to sign it. " +
			"It cannot propose, sign or send anything itself."
	}
	return r.reply(ctx, fmt.Sprintf(
		"Token #%d, %q. This is the only time I'll show it:\n\n%s\n\n%s "+
			"/agent-revoke %d withdraws it.",
		record.ID, record.Label, token, can, record.ID))
}

// agentTokens lists the workspace's tokens, without the tokens.
func (s *Service) agentTokens(ctx context.Context, r *request) error {
	list, err := s.store.MCPTokens(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "No agent tokens here. /agent-token <label> mints one.")
	}
	var b strings.Builder
	b.WriteString("Agent tokens:\n")
	now := time.Now()
	for _, t := range list {
		state := "never used"
		if t.LastUsedAt != nil {
			state = "last used " + t.LastUsedAt.UTC().Format("2 Jan 15:04 MST")
		}
		if !t.Live(now) {
			state = "revoked"
		}
		fmt.Fprintf(&b, "\n#%d · %s · %s · %s", t.ID, t.Label, t.Tier, state)
	}
	return r.reply(ctx, b.String())
}

// agentRevoke withdraws a token.
func (s *Service) agentRevoke(ctx context.Context, r *request) error {
	arg := strings.TrimPrefix(argument(r.msg, "id"), "#")
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 {
		return r.reply(ctx, "Which token? Try: /agent-revoke 3 — /agent-tokens lists them.")
	}
	err = s.store.RevokeMCPToken(ctx, r.org.ID, id)
	switch {
	case errors.Is(err, store.ErrNoToken):
		return r.reply(ctx, fmt.Sprintf("There's no live token #%d here.", id))
	case err != nil:
		return err
	}
	s.log.Info("agent token revoked", "org", r.org.ID, "token", id, "by", r.member.ID)
	return r.reply(ctx, fmt.Sprintf("Token #%d is revoked. Anything using it stops working now.", id))
}

// ---- Sending in your own words, as a command.

// send hands a sentence to the same path a plain message takes.
//
// On Telegram a message with no command already goes there. Discord delivers
// nothing to a bot but slash commands, so without this the whole free-text path
// does not exist on Discord at all.
func (s *Service) send(ctx context.Context, r *request) error {
	what := argument(r.msg, "what")
	if what == "" {
		return r.reply(ctx, "Tell me what to send, in your own words. Try: /send 50 to mum")
	}
	m := r.msg
	// The sentence is what gets tokenized, and every span the decoder returns
	// is checked against it. The command word belongs in it: the person typed
	// "/send 50 to mum", and "send" is the verb of that instruction. Handing
	// over "50 to mum" alone gives the decoder an amount and a recipient and
	// nothing that says what to do with them, and it rightly declines to guess.
	m.Command, m.Args = "", "send "+what
	return s.sender.HandleSend(ctx, r.scope, m, r.out, r.links)
}

// workspaceCommands are registered alongside the rest.
func (s *Service) workspaceCommands() []*command {
	text := func(name, description string, required bool) []chat.Option {
		return []chat.Option{{Name: name, Description: description, Type: chat.OptString, Required: required}}
	}
	return []*command{
		{
			name:        commandConnect,
			description: "Add another server or group to this workspace",
			options:     text("code", "The code from /connect in the space that is already set up", false),
			// No minRole, for the same reason as setup: one of its two halves
			// runs where no workspace, and so no role, exists yet. The half
			// that runs inside a workspace checks for admin itself.
			run: func(ctx context.Context, s *Service, r *request) error { return s.connect(ctx, r) },
		},
		{
			name:        "send",
			needs:       needsFreeText,
			description: "Send from your own wallet, in your own words",
			options:     text("what", "Like: 50 to mum", true),
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.send(ctx, r) },
		},
		{
			name:        "save",
			description: "Save an address under a name, for your own messages",
			options: []chat.Option{
				{Name: "name", Description: "What you'll call them", Type: chat.OptString, Required: true},
				{Name: "address", Description: "Their G… address", Type: chat.OptString, Required: true},
			},
			minRole: chat.RoleNone,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.save(ctx, r) },
		},
		{
			name:        "saved",
			description: "List the recipients you have saved",
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.saved(ctx, r) },
		},
		{
			name:        "forget",
			description: "Remove a saved recipient",
			options:     text("name", "The name you saved them under", true),
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.forget(ctx, r) },
		},
		{
			name:        "limits",
			description: "Show or set spend limits on treasury proposals",
			options: []chat.Option{
				{Name: "payment", Description: "Largest single proposal, or none", Type: chat.OptString},
				{Name: "daily", Description: "Most proposed in 24 hours, or none", Type: chat.OptString},
			},
			// Observer to look; changing them is checked inside.
			minRole: chat.RoleObserver,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.limits(ctx, r) },
		},
		{
			name:        "agent-token",
			description: "Mint a token for an outside agent",
			options: []chat.Option{
				{Name: "label", Description: "What it is for", Type: chat.OptString, Required: true},
				{Name: "tier", Description: "read, or propose to let it write drafts", Type: chat.OptString},
			},
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.agentToken(ctx, r) },
		},
		{
			name:        "agent-tokens",
			description: "List this workspace's agent tokens",
			minRole:     chat.RoleAdmin,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.agentTokens(ctx, r) },
		},
		{
			name:        "agent-revoke",
			description: "Withdraw an agent token",
			options:     text("id", "The token's number, from /agent-tokens", true),
			minRole:     chat.RoleAdmin,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.agentRevoke(ctx, r) },
		},
	}
}
