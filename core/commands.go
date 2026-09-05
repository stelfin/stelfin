package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// Command names, as constants where the router needs to compare against one.
const commandSetup = "setup"

// command is one thing the bot can be asked to do.
type command struct {
	name        string
	description string
	options     []chat.Option
	// minRole is the org role required. Checked in Go on every invocation: a
	// platform's own permission system hides a command it does not want shown,
	// but a guild administrator can edit those, and Telegram's command scopes
	// are only a hint to the autocomplete menu.
	minRole chat.Role
	run     func(ctx context.Context, s *Service, r *request) error
}

func (s *Service) registerCommands() {
	cmds := []*command{
		{
			name:        commandSetup,
			description: "Register this space with stelfin",
			options: []chat.Option{{
				Name: "name", Description: "What to call this workspace",
				Type: chat.OptString, Required: true,
			}},
			// No minRole: setup has to run before any org role can exist, so
			// its gate is platform-level authority over the space, checked
			// inside the handler on both the registered and unregistered paths.
			run: func(ctx context.Context, s *Service, r *request) error { return s.setupHere(ctx, r) },
		},
		{
			name:        "whoami",
			description: "Show who stelfin thinks you are here",
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.whoami(ctx, r) },
		},
		{
			name:        "help",
			description: "List what stelfin can do",
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.help(ctx, r) },
		},
	}

	cmds = append(cmds, s.linkCommands()...)
	cmds = append(cmds, s.treasuryCommands()...)
	cmds = append(cmds, s.proposalCommands()...)
	cmds = append(cmds, s.reclaimCommands()...)

	s.commands = make(map[string]*command, len(cmds))
	s.catalog = make([]chat.Command, 0, len(cmds))
	for _, c := range cmds {
		s.commands[c.name] = c
		s.catalog = append(s.catalog, chat.Command{
			Name:        c.name,
			Description: c.description,
			Options:     c.options,
			MinRole:     c.minRole,
		})
	}
	sort.Slice(s.catalog, func(i, j int) bool { return s.catalog[i].Name < s.catalog[j].Name })
}

// runSetup handles /setup in a space with no org behind it.
//
// This is the only command that can run before a tenant exists, and it is the
// only path that creates one.
func (s *Service) runSetup(ctx context.Context, m chat.Inbound, out api.Replier) error {
	r := &request{msg: m, actor: m.Actor, out: out}
	return s.setupHere(ctx, r)
}

// setupHere registers a space, creating the org if there is not one already.
func (s *Service) setupHere(ctx context.Context, r *request) error {
	// Platform-level authority, asked of the platform rather than read from the
	// message — on a transport whose deliveries do not authenticate their own
	// contents, a claim inside one is worth nothing.
	admin, err := s.admins.IsSpaceAdmin(ctx, r.msg.Conversation, r.actor)
	if err != nil {
		s.log.Warn("could not establish space admin", "error", err)
		return r.reply(ctx, "I couldn't check whether you administer this space. Try again in a moment.")
	}
	if !admin {
		return r.reply(ctx, "Only an administrator of this space can run /setup.")
	}
	if r.msg.Conversation.IsDM {
		return r.reply(ctx, "Run /setup in the group or server the workspace should belong to, not in a direct message.")
	}

	// An org already registered here is a repeated /setup, which is a no-op
	// rather than an error: someone running it twice should be told where they
	// stand, not shown a failure.
	if r.org.ID != 0 {
		return r.reply(ctx, fmt.Sprintf(
			"This space is already set up as %q. Run /whoami to see where you stand.",
			r.org.DisplayName))
	}

	name := strings.TrimSpace(r.msg.Options["name"])
	if name == "" {
		name = strings.TrimSpace(r.msg.Args)
	}
	if name == "" {
		return r.reply(ctx, "What should I call this workspace? Try: /setup Acme DAO")
	}
	if len([]rune(name)) > 64 {
		return r.reply(ctx, "That name is too long. Sixty-four characters or fewer, please.")
	}

	org, err := s.store.CreateOrg(ctx, store.CreateOrgParams{
		Slug:        slugify(name, r.msg.Conversation.SpaceID),
		DisplayName: name,
		Network:     s.network,
		Channel:     r.msg.Conversation.Channel,
		SpaceID:     r.msg.Conversation.SpaceID,
		InstalledBy: r.actor.Ref(),
	})
	switch {
	case errors.Is(err, store.ErrSpaceRegistered):
		// Two administrators ran /setup at the same moment. The other one won.
		return r.reply(ctx, "This space was just set up by someone else. Run /whoami to see where you stand.")
	case errors.Is(err, store.ErrSlugTaken):
		return r.reply(ctx, "A workspace with a very similar name already exists. Try a different name.")
	case err != nil:
		return err
	}

	// Whoever set the space up is its first admin. Somebody has to be, and the
	// alternative is a workspace nobody can administer.
	member, err := s.store.EnsureMember(ctx, org.ID,
		r.actor.Channel, r.actor.UserID, r.actor.Handle)
	if err != nil {
		return err
	}
	if err := s.store.GrantRole(ctx, org.ID, member.ID, chat.RoleAdmin, member.ID); err != nil {
		return err
	}

	s.log.Info("workspace created",
		"org", org.ID, "channel", r.msg.Conversation.Channel,
		"space", r.msg.Conversation.SpaceID, "by", r.actor.Ref())

	return r.reply(ctx, fmt.Sprintf(
		"%s is set up, on %s, and you're its first admin.\n\n"+
			"Nothing can move money yet — this workspace has no treasury linked. "+
			"Run /whoami to see where you stand.",
		org.DisplayName, org.Network))
}

// whoami reports what stelfin knows about the person asking.
//
// Deliberately explicit about the distinction the whole model rests on: a role
// says what the bot will offer, not what the network will accept.
func (s *Service) whoami(ctx context.Context, r *request) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s (%s)\n", r.org.DisplayName, r.org.Network)
	fmt.Fprintf(&b, "You: %s\n", r.actor.Ref())

	if r.role == chat.RoleNone {
		b.WriteString("Role: none yet — an admin here can give you one.\n")
	} else {
		fmt.Fprintf(&b, "Role: %s\n", r.role)
	}

	if r.member.HasAddress() {
		fmt.Fprintf(&b, "Wallet: %s (%s)\n", r.member.Address, r.member.AddressSource)
	} else {
		b.WriteString("Wallet: not linked yet.\n")
	}

	b.WriteString("\nYour role decides what I'll offer you. " +
		"Whether an approval counts is decided on chain, by the treasury's own signers.")
	return r.reply(ctx, b.String())
}

// help lists what the person asking can actually do, rather than everything
// that exists.
func (s *Service) help(ctx context.Context, r *request) error {
	var b strings.Builder
	b.WriteString("What I can do here:\n")
	for _, c := range s.catalog {
		cmd := s.commands[c.Name]
		if !r.role.AtLeast(cmd.minRole) {
			continue
		}
		fmt.Fprintf(&b, "  /%s — %s\n", c.Name, c.Description)
	}
	b.WriteString("\nOr just tell me what you want to send, in your own words.")
	return r.reply(ctx, b.String())
}

// slugify derives a schema-valid org slug from a display name.
//
// The space id is mixed in rather than trusted to be absent from someone else's
// name: two DAOs both called "Treasury" are ordinary, and the slug is a unique
// key. Names are also user input in whatever script they please, so a name that
// reduces to nothing still has to produce something usable.
func slugify(name, spaceID string) string {
	var b strings.Builder
	var lastDash bool
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 24 {
		base = strings.Trim(base[:24], "-")
	}
	if base == "" {
		base = "dao"
	}

	suffix := onlySlugChars(spaceID)
	if len(suffix) > 12 {
		suffix = suffix[len(suffix)-12:]
	}
	if suffix == "" {
		suffix = "0"
	}
	return base + "-" + suffix
}

func onlySlugChars(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
