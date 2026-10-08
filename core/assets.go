package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// What a treasury holds, and what it can be asked to hold or exchange.

// balance shows what the treasuries and the caller's own wallets hold.
//
// Anybody may ask what they hold themselves. What the treasury holds is public
// on the network to anybody with its address, so hiding it here would protect
// nothing — but a bot answers the people it has been told about, and somebody
// with no role in a workspace has not been told about its treasury by this one.
func (s *Service) balance(ctx context.Context, r *request) error {
	observer := r.role.AtLeast(chat.RoleObserver)
	linked := ""
	if r.member.HasAddress() {
		linked = r.member.Address
	}
	report, err := s.sender.Balances(ctx, r.scope, observer, linked)
	if err != nil {
		return err
	}

	var b strings.Builder
	account := func(heading string, a api.AccountBalance) {
		fmt.Fprintf(&b, "%s\n%s\n", heading, a.Address)
		switch {
		case a.Contract:
			b.WriteString("  held by a contract; its balance is not shown here yet\n")
		case !a.Exists:
			b.WriteString("  not on the network yet: it has an address, and no account until somebody funds it\n")
		case len(a.Lines) == 0:
			b.WriteString("  nothing\n")
		}
		for _, line := range a.Lines {
			fmt.Fprintf(&b, "  %s %s", line.Amount.Display(), line.Asset)
			if !line.Authorized {
				b.WriteString(" (frozen by its issuer)")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	for _, t := range report.Treasuries {
		heading := "Treasury"
		if t.Label != "" {
			heading = "Treasury — " + t.Label
		}
		account(heading, t)
	}
	for _, w := range report.Wallets {
		account("Your wallet ("+w.Label+")", w)
	}

	switch {
	case observer && len(report.Treasuries) == 0:
		b.WriteString("No treasury is linked here yet. An admin can run /link-treasury GABC… first.\n\n")
	case !observer:
		b.WriteString("The treasury's balance is shown to members with a role here. " +
			"An admin can give you one with /grant.\n\n")
	}
	if len(report.Wallets) == 0 {
		b.WriteString("You have no wallet here yet. /link one you already have.\n\n")
	}
	b.WriteString("Read from the network just now.")
	return r.reply(ctx, b.String())
}

// trust proposes opening a trustline, and untrust closing one.
func (s *Service) trust(ctx context.Context, r *request, open bool) error {
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}
	asset := strings.TrimSpace(r.msg.Options["asset"])
	if asset == "" {
		asset = strings.TrimSpace(r.msg.Args)
	}
	if asset == "" {
		if open {
			return r.reply(ctx, "Which asset? Name it with its issuer: /trust EURC:GABC…\n\n"+
				"The issuer is what makes it that asset and not another with the same name.")
		}
		return r.reply(ctx, "Which asset? Try: /untrust EURC")
	}

	var view *api.ProposalView
	var name string
	if open {
		view, name, err = s.sender.ProposeTrust(ctx, r.scope, treasury.ID, asset, r.member.ID)
	} else {
		view, name, err = s.sender.ProposeUntrust(ctx, r.scope, treasury.ID, asset, r.member.ID)
	}
	if reply, handled, replyErr := s.proposalRefusal(ctx, r, treasury, err); handled || replyErr != nil {
		if replyErr != nil {
			return replyErr
		}
		return r.reply(ctx, reply)
	}

	what := fmt.Sprintf("It lets %s hold %s, and locks 0.5 XLM of the treasury's reserve for as long "+
		"as the trustline stays open. It moves nothing.", treasuryName(treasury), name)
	if !open {
		what = fmt.Sprintf("It closes %s's trustline for %s and releases the 0.5 XLM it locked. "+
			"Afterwards the treasury cannot be paid in %s until one is opened again.",
			treasuryName(treasury), name, name)
	}
	return s.announce(ctx, r, treasury, view, what)
}

// swap quotes a swap, or proposes one once a floor has been named.
//
// Two steps on purpose, and the first proposes nothing. The floor — the least
// the treasury will accept — is the whole of a swap's risk, and nobody can
// choose it without seeing what the market offers. So asking with no floor
// gets the market's answer and the two ways to state one; asking with a floor
// gets a proposal. There is no default floor, because a default would be a
// number this bot chose about somebody else's money.
func (s *Service) swap(ctx context.Context, r *request) error {
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}
	args := swapArguments(r.msg)
	if args["amount"] == "" || args["from"] == "" || args["to"] == "" {
		return r.reply(ctx, "Try: /swap 100 XLM to USDC\n\n"+
			"That tells you what the market offers right now and proposes nothing. To propose it, "+
			"add the least you'd accept — min:12 — or how far below the market you'd go — slippage:1%.")
	}
	amount, err := money.Parse(args["amount"])
	if err != nil || amount <= 0 {
		return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount to swap.", args["amount"]))
	}

	var minimum money.Stroops
	var slippage int64
	if text := args["min"]; text != "" {
		if minimum, err = money.Parse(text); err != nil || minimum <= 0 {
			return r.reply(ctx, fmt.Sprintf("I couldn't read %q as the least you'd accept.", text))
		}
	}
	if text := args["slippage"]; text != "" {
		if slippage, err = basisPoints(text); err != nil {
			return r.reply(ctx, fmt.Sprintf(
				"I couldn't read %q as a tolerance. Write it as a percentage under 50, like slippage:1%%.", text))
		}
	}
	if minimum != 0 && slippage != 0 {
		return r.reply(ctx, "Give one or the other: min: is the least you'd accept, and "+
			"slippage: works it out from the market. Both would be two answers to one question.")
	}

	// No floor named: say what the market offers, and propose nothing.
	if minimum == 0 && slippage == 0 {
		quote, err := s.sender.QuoteSwap(ctx, r.scope, treasury.ID, amount, args["from"], args["to"])
		if reply, handled, replyErr := s.proposalRefusal(ctx, r, treasury, err); handled || replyErr != nil {
			if replyErr != nil {
				return replyErr
			}
			return r.reply(ctx, reply)
		}
		return r.reply(ctx, fmt.Sprintf(
			"Right now %s %s would bring %s about %s %s%s.\n\n"+
				"Nothing has been proposed. A market moves, so a swap states the least it will accept "+
				"and fails rather than take less. To propose this one, say that floor either way:\n\n"+
				"/swap %s %s to %s min:%s\n"+
				"/swap %s %s to %s slippage:1%%",
			amount.Display(), quote.FromName, treasuryName(treasury), quote.Receive.Display(), quote.ToName,
			via(quote.Path),
			args["amount"], args["from"], args["to"], floorExample(quote.Receive),
			args["amount"], args["from"], args["to"]))
	}

	view, quote, floor, err := s.sender.ProposeSwap(ctx, r.scope, api.SwapParams{
		Treasury: treasury.ID, Amount: amount, From: args["from"], To: args["to"],
		Minimum: minimum, SlippageBps: slippage, CreatedBy: r.member.ID,
	})
	if reply, handled, replyErr := s.proposalRefusal(ctx, r, treasury, err); handled || replyErr != nil {
		if replyErr != nil {
			return replyErr
		}
		return r.reply(ctx, reply)
	}
	return s.announce(ctx, r, treasury, view, fmt.Sprintf(
		"It sends exactly %s %s from %s and brings back at least %s %s — the market offered about %s "+
			"when this was built. If it cannot deliver that floor when it is submitted, it is not "+
			"submitted, and nothing is spent.",
		amount.Display(), quote.FromName, treasuryName(treasury), floor.Display(), quote.ToName,
		quote.Receive.Display()))
}

// announce replies with a new proposal and the proposer's own link to sign it.
func (s *Service) announce(
	ctx context.Context, r *request, treasury store.Treasury, view *api.ProposalView, what string,
) error {
	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}
	// What the envelope does, read back out of the envelope, beside what was
	// asked for. The two should agree, and showing only the second would hide
	// it on the day they do not.
	does := ""
	if view.Description != nil && len(view.Description.Operations) == 1 {
		does = ": " + view.Description.Operations[0].Summary
	}
	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d, from %s%s\n\n%s\n\n"+
			"It needs signing weight %d and has %d. Sign it here:\n%s\n\n"+
			"Everyone else signing runs /approve %d to get their own link — this one is yours. "+
			"Anyone can run /execute %d once it has the weight, and /cancel %d withdraws it.",
		view.Proposal.ID, treasuryName(treasury), does, what,
		view.Need, view.Have, link,
		view.Proposal.ID, view.Proposal.ID, view.Proposal.ID))
}

// proposalRefusal turns what can stop a proposal being opened into a reply.
// handled is false when err is nil or is not something to explain.
func (s *Service) proposalRefusal(
	ctx context.Context, r *request, treasury store.Treasury, err error,
) (reply string, handled bool, failed error) {
	var refusal *api.RefusalError
	var over *api.CeilingError
	var moved *api.MarketMovedError
	switch {
	case err == nil:
		return "", false, nil
	case errors.As(err, &refusal):
		return "I haven't put that to the signers: " + refusal.Reason + ".", true, nil
	case errors.As(err, &over):
		return overCeilingReply(over), true, nil
	case errors.Is(err, settlement.ErrNoPath):
		return "Nobody on this network is offering that exchange right now, so there is no price " +
			"to quote and nothing to propose. On testnet an order book exists only where somebody " +
			"has placed an order.", true, nil
	case errors.As(err, &moved):
		return marketMovedReply(moved), true, nil
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		open, found, findErr := s.store.OpenProposal(ctx, r.org.ID, treasury.ID)
		if findErr != nil {
			return "", false, findErr
		}
		if !found {
			return "That exact proposal has already been made here and is closed now. " +
				"Try again in a moment and it will be a fresh one.", true, nil
		}
		return fmt.Sprintf(
			"%s already has proposal #%d open, and one account can only have one at a time — both "+
				"would reserve the same sequence number, and whichever went first would silently "+
				"kill the other.\n\nFinish it with /approve %d and /execute %d, or withdraw it with /cancel %d.",
			treasuryName(treasury), open.ID, open.ID, open.ID, open.ID), true, nil
	case errors.Is(err, api.ErrReadOnly):
		return readOnlyReply, true, nil
	}
	return "", false, err
}

// marketMovedReply says a swap would fail now, and that nothing was done.
func marketMovedReply(e *api.MarketMovedError) string {
	now := fmt.Sprintf("the market now offers about %s %s", e.Now.Display(), e.Asset)
	if e.Now == 0 {
		now = "nobody is offering that exchange at the moment"
	}
	return fmt.Sprintf(
		"Not submitted: %s, and this swap will not take less than %s. Submitting it now would fail "+
			"on the ledger and use up its signatures.\n\nNothing was spent and nothing has changed. "+
			"Try /execute again when the market is back, or /cancel it and propose one with a floor "+
			"the market can meet.", now, e.Minimum.Display())
}

// swapArguments reads /swap's arguments from wherever the platform put them.
//
// Discord delivers them as options. Telegram delivers a word and some text, so
// there they are read by a fixed grammar: an amount, an asset, "to", an asset,
// then min: or slippage:. Fixed, and not a guess, for the reason /pay's is.
func swapArguments(m chat.Inbound) map[string]string {
	out := make(map[string]string, 5)
	for _, key := range []string{"amount", "from", "to", "min", "slippage", "treasury"} {
		if v := strings.TrimSpace(m.Options[key]); v != "" {
			out[key] = v
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, w := range strings.Fields(m.Args) {
		if key, value, ok := strings.Cut(w, ":"); ok {
			switch key = strings.ToLower(key); key {
			case "min", "slippage", "treasury":
				out[key] = value
				continue
			}
			// Otherwise it is CODE:ISSUER, which is an asset and not an option.
		}
		switch {
		case out["amount"] == "":
			out["amount"] = w
		case out["from"] == "":
			out["from"] = w
		case out["to"] == "":
			if strings.EqualFold(w, "to") || strings.EqualFold(w, "for") {
				continue
			}
			out["to"] = w
		}
	}
	return out
}

// basisPoints reads a tolerance written as a percentage: "1%", "0.5", "2.25%".
//
// Under fifty. A tolerance of half the value or more is not a tolerance, it is
// a typo, and the one place a typo here costs money is the floor of a swap.
func basisPoints(text string) (int64, error) {
	text = strings.TrimSuffix(strings.TrimSpace(text), "%")
	percent, err := strconv.ParseFloat(text, 64)
	if err != nil || percent <= 0 || percent >= 50 {
		return 0, fmt.Errorf("core: %q is not a tolerance between 0 and 50 percent", text)
	}
	bps := int64(percent*100 + 0.5)
	if bps <= 0 {
		return 0, fmt.Errorf("core: %q is too small to be a tolerance", text)
	}
	return bps, nil
}

// via names the assets a swap passes through, or nothing when it is direct.
func via(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return ", by way of " + strings.Join(path, " then ")
}

// floorExample is a floor a little under the quote, for the example command: a
// number somebody can see is theirs to change, not one to copy without reading.
func floorExample(quote money.Stroops) string {
	floor := quote - quote/50
	if floor <= 0 {
		floor = quote
	}
	return strings.ReplaceAll(floor.Display(), ",", "")
}

func (s *Service) assetCommands() []*command {
	return []*command{
		{
			name:        "balance",
			description: "Show what the treasury and your own wallet hold",
			minRole:     chat.RoleNone,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.balance(ctx, r) },
		},
		{
			name:        "trust",
			description: "Propose that the treasury can hold an asset",
			options: []chat.Option{
				{Name: "asset", Description: "CODE:ISSUER, like EURC:GABC…", Type: chat.OptString, Required: true},
				{Name: "treasury", Description: "Which treasury, if there are several", Type: chat.OptString},
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.trust(ctx, r, true) },
		},
		{
			name:        "untrust",
			description: "Propose closing one of the treasury's trustlines",
			options: []chat.Option{
				{Name: "asset", Description: "The asset to stop holding", Type: chat.OptString, Required: true},
				{Name: "treasury", Description: "Which treasury, if there are several", Type: chat.OptString},
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.trust(ctx, r, false) },
		},
		{
			name:        "swap",
			description: "Quote, then propose, swapping one asset the treasury holds for another",
			options: []chat.Option{
				{Name: "amount", Description: "How much to send", Type: chat.OptString, Required: true},
				{Name: "from", Description: "The asset to send: XLM, USDC, or CODE:ISSUER", Type: chat.OptString, Required: true},
				{Name: "to", Description: "The asset to receive", Type: chat.OptString, Required: true},
				{Name: "min", Description: "The least to accept. Leave out for a quote only", Type: chat.OptString},
				{Name: "slippage", Description: "Or how far below the market to go, like 1%", Type: chat.OptString},
				{Name: "treasury", Description: "Which treasury, if there are several", Type: chat.OptString},
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.swap(ctx, r) },
		},
	}
}
