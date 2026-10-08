package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
)

// membersListed bounds /members. A reply is one chat message, and a roster
// longer than this is not something anybody reads in one.
const membersListed = 50

// grant sets the role one member holds here.
//
// The member is named by number, the one /whoami and /members print, and never
// by handle. A handle can be renamed and, once released, taken by someone else;
// a role granted to "@ada" would follow the name to whoever holds it next.
//
// What this hands out is the bot's attention, not authority over money. An
// approver is someone the bot will offer a signing link to; whether their
// signature counts is the treasury account's business, and no role granted here
// can make it count.
func (s *Service) grant(ctx context.Context, r *request) error {
	who, roleName := roleArguments(r.msg)
	if picked := strings.TrimSpace(r.msg.Options["user"]); picked != "" && roleName == "" {
		// "/grant approver" in reply to someone: the one word is the role.
		roleName, who = who, ""
	}
	if picked := strings.TrimSpace(r.msg.Options["user"]); picked != "" {
		// Chosen from the platform's own member picker, which hands over the
		// account's immutable id rather than its name — so this is as safe as a
		// member number, and does not need the person to have spoken first.
		member, err := s.store.EnsureMember(ctx, r.org.ID, r.actor.Channel, picked, "")
		if err != nil {
			return err
		}
		who = strconv.FormatInt(int64(member.ID), 10)
	}
	if who == "" || roleName == "" {
		return r.reply(ctx,
			"Try: /grant 12 approver\n\n"+
				"The number is the member's — they can read it off /whoami, and "+
				"/members lists everyone I've seen here. Or send /grant approver as a "+
				"reply to something they wrote. Roles: observer, proposer, approver, admin.")
	}

	role, ok := roleNamed(roleName)
	if !ok {
		return r.reply(ctx, fmt.Sprintf(
			"I don't know a role called %q. They are: observer, proposer, approver, admin.",
			roleName))
	}
	return s.setRole(ctx, r, who, role)
}

// revoke removes every role a member holds here.
func (s *Service) revoke(ctx context.Context, r *request) error {
	who, _ := roleArguments(r.msg)
	if picked := strings.TrimSpace(r.msg.Options["user"]); picked != "" && who == "" {
		member, found, err := s.store.MemberByIdentity(ctx, r.org.ID, r.actor.Channel, picked)
		if err != nil {
			return err
		}
		if !found {
			return r.reply(ctx, "That person has no role here to take away.")
		}
		who = strconv.FormatInt(int64(member.ID), 10)
	}
	if who == "" {
		return r.reply(ctx, "Try: /revoke 12 — the number /members lists them under.")
	}
	return s.setRole(ctx, r, who, chat.RoleNone)
}

func (s *Service) setRole(ctx context.Context, r *request, who string, role chat.Role) error {
	n, err := strconv.ParseInt(strings.TrimPrefix(who, "#"), 10, 64)
	if err != nil || n <= 0 {
		return r.reply(ctx, fmt.Sprintf(
			"I couldn't read %q as a member number. /members lists them.", who))
	}
	id := store.MemberID(n)

	err = s.store.SetRole(ctx, r.org.ID, id, role, r.member.ID)
	switch {
	case errors.Is(err, store.ErrMemberNotFound):
		return r.reply(ctx, fmt.Sprintf(
			"There's no member #%d here. Someone becomes a member the first time "+
				"they speak to me in this workspace — /whoami will do.", id))
	case errors.Is(err, store.ErrLastAdmin):
		return r.reply(ctx,
			"That would leave this workspace with no admin, and then nobody could "+
				"ever grant a role here again. Make someone else an admin first.")
	case err != nil:
		return err
	}

	s.log.Info("role changed", "org", r.org.ID, "member", id, "role", role.String(), "by", r.member.ID)

	target, err := s.store.Member(ctx, r.org.ID, id)
	if err != nil {
		return err
	}
	if role == chat.RoleNone {
		return r.reply(ctx, fmt.Sprintf("%s has no role here now.", memberName(target)))
	}
	return r.reply(ctx, fmt.Sprintf(
		"%s is now %s here.\n\n"+
			"That decides what I'll offer them. Whether a signature of theirs counts "+
			"is decided on chain, by the treasury's own signers.",
		memberName(target), withArticle(role)))
}

// members lists who the workspace has seen and what each may ask for.
func (s *Service) members(ctx context.Context, r *request) error {
	list, err := s.store.Members(ctx, r.org.ID, membersListed+1)
	if err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Members of %s:\n", r.org.DisplayName)
	for i, m := range list {
		if i == membersListed {
			b.WriteString("\n…and more.")
			break
		}
		wallet := "no wallet linked"
		if m.Member.HasAddress() {
			wallet = m.Member.Address
		}
		fmt.Fprintf(&b, "\n%s · %s · %s", memberName(m.Member), m.Role, wallet)
	}
	b.WriteString("\n\n/grant <number> <role> changes one.")
	return r.reply(ctx, b.String())
}

// roleArguments reads a member and a role from wherever the platform put them:
// structured options on Discord, the argument text on Telegram.
func roleArguments(m chat.Inbound) (who, role string) {
	who = strings.TrimSpace(m.Options["member"])
	role = strings.TrimSpace(m.Options["role"])
	if who != "" || role != "" {
		return who, role
	}
	words := strings.Fields(m.Args)
	if len(words) > 0 {
		who = words[0]
	}
	if len(words) > 1 {
		role = words[1]
	}
	return who, role
}

// roleNamed parses a role a person typed. "none" is deliberately not one:
// taking a role away is /revoke, so that it cannot happen by a typo in /grant.
func roleNamed(name string) (chat.Role, bool) {
	for _, r := range []chat.Role{chat.RoleObserver, chat.RoleProposer, chat.RoleApprover, chat.RoleAdmin} {
		if strings.EqualFold(name, r.String()) {
			return r, true
		}
	}
	return chat.RoleNone, false
}

// memberName is how a member is referred to in a reply: by number, which is
// what identifies them, with the display name beside it for the reader.
func memberName(m store.Member) string {
	if m.DisplayName == "" {
		return fmt.Sprintf("#%d", m.ID)
	}
	return fmt.Sprintf("#%d (%s)", m.ID, m.DisplayName)
}

func withArticle(r chat.Role) string {
	switch r {
	case chat.RoleObserver, chat.RoleApprover, chat.RoleAdmin:
		return "an " + r.String()
	default:
		return "a " + r.String()
	}
}

// roleCommands are registered alongside the rest.
func (s *Service) roleCommands() []*command {
	return []*command{
		{
			name:        "grant",
			description: "Give a member a role here",
			// Required options first, which Discord insists on. Then two ways
			// of saying who: the platform's picker where there is one, and the
			// member number everywhere.
			options: []chat.Option{
				{Name: "role", Description: "observer, proposer, approver or admin", Type: chat.OptString, Required: true},
				{Name: "user", Description: "Who, picked from this server", Type: chat.OptUser},
				{Name: "member", Description: "Or their member number, from /whoami", Type: chat.OptString},
			},
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.grant(ctx, r) },
		},
		{
			name:        "revoke",
			description: "Take a member's role away",
			options: []chat.Option{
				{Name: "member", Description: "Their member number, from /members", Type: chat.OptString, Required: true},
			},
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.revoke(ctx, r) },
		},
		{
			name:        "members",
			description: "List members and their roles",
			minRole:     chat.RoleAdmin,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.members(ctx, r) },
		},
	}
}
