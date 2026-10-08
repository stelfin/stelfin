package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// defaultDailyWallets is how many wallets a workspace may have set up in a day
// when an admin turns provisioning on without saying.
const defaultDailyWallets = 25

// maxDailyWallets bounds what an admin can ask for. The deployment has its own
// daily ceiling across every workspace, and a number above it would be a
// promise this one could not keep.
const maxDailyWallets = store.GlobalDailyGrants

// provisioning shows or changes whether this workspace has wallets set up for
// members who do not bring their own.
//
// It is the operator's money being spent — every wallet locks reserve out of
// the deployment's float — which is why it starts off, why an admin has to ask
// for it, and why asking is not enough: nothing is provisioned for a workspace
// that has not proved it controls a treasury. That proof takes real signatures,
// which is the one thing somebody adding the bot to a throwaway group to drain
// the float does not have.
func (s *Service) provisioning(ctx context.Context, r *request) error {
	words := strings.Fields(strings.ToLower(r.msg.Args))
	state := strings.ToLower(strings.TrimSpace(r.msg.Options["state"]))
	if state == "" && len(words) > 0 {
		state = words[0]
	}
	perDay := strings.TrimSpace(r.msg.Options["per-day"])
	if perDay == "" && len(words) > 1 {
		perDay = words[1]
	}

	policy, err := s.store.EnrollmentPolicyOf(ctx, r.org.ID)
	if err != nil {
		return err
	}

	switch state {
	case "":
		return r.reply(ctx, describeProvisioning(policy))

	case "off":
		if err := s.store.SetEnrollmentPolicy(ctx, r.org.ID, 0, policy.DailyCap); err != nil {
			return err
		}
		s.log.Info("provisioning turned off", "org", r.org.ID, "by", r.member.ID)
		return r.reply(ctx,
			"Provisioning is off. No new wallets will be set up here.\n\n"+
				"Wallets already set up keep working, and their owners can still hand "+
				"them back with /reclaim.")

	case "on":
		cap := defaultDailyWallets
		if perDay != "" {
			n, err := strconv.Atoi(perDay)
			if err != nil || n < 1 || n > maxDailyWallets {
				return r.reply(ctx, fmt.Sprintf(
					"I couldn't read %q as a number of wallets a day. Between 1 and %d, "+
						"like: /provisioning on 10", perDay, maxDailyWallets))
			}
			cap = n
		}
		if !policy.HasTreasury {
			// Refused here rather than recorded and left inert: a setting that
			// says "on" while nothing can be provisioned is how an admin ends
			// up telling members to try something that will not work.
			return r.reply(ctx,
				"Link a treasury first, with /link-treasury.\n\n"+
					"Setting up wallets spends the operator's XLM, so it's only "+
					"available to a workspace that has proved it controls one.")
		}
		budget := api.ProvisionReserve * money.Stroops(cap)
		if err := s.store.SetEnrollmentPolicy(ctx, r.org.ID, budget, cap); err != nil {
			return err
		}
		s.log.Info("provisioning turned on", "org", r.org.ID, "per_day", cap, "by", r.member.ID)
		return r.reply(ctx, fmt.Sprintf(
			"Provisioning is on: up to %d wallets a day.\n\n"+
				"A member who messages me in their own words and has no wallet is "+
				"offered one. Each locks %s XLM of the operator's reserve until its "+
				"owner hands it back with /reclaim. Someone I first saw in the last "+
				"few minutes has to wait a little.",
			cap, api.ProvisionReserve))

	default:
		return r.reply(ctx, "Try: /provisioning on, /provisioning on 10, or /provisioning off.")
	}
}

func describeProvisioning(p store.EnrollmentPolicy) string {
	switch {
	case p.On():
		return fmt.Sprintf(
			"Provisioning is on: up to %d wallets a day. /provisioning off turns it off.",
			p.DailyCap)
	case !p.HasTreasury:
		return "Provisioning is off. It needs a linked treasury first — /link-treasury — " +
			"and then /provisioning on."
	default:
		return "Provisioning is off. /provisioning on turns it on."
	}
}

// provisioningCommands are registered alongside the rest.
func (s *Service) provisioningCommands() []*command {
	return []*command{{
		name:        "provisioning",
		description: "Turn wallet setup for members on or off",
		options: []chat.Option{
			{Name: "state", Description: "on or off; leave out to see where it stands", Type: chat.OptString},
			{Name: "per-day", Description: "How many wallets a day, when turning it on", Type: chat.OptString},
		},
		minRole: chat.RoleAdmin,
		run:     func(ctx context.Context, s *Service, r *request) error { return s.provisioning(ctx, r) },
	}}
}
