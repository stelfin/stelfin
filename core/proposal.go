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
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// approveLinkLifetime bounds how long an approver's link is usable.
//
// Shorter than the proposal it names, so a link left in a scrollback stops
// working while the proposal is still collecting signatures — asking again is
// one command, and a link that outlives its usefulness is one more thing
// carrying authority for no reason.
const approveLinkLifetime = 30 * time.Minute

// pay puts a treasury payment to the account's signers.
func (s *Service) pay(ctx context.Context, r *request) error {
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}

	amountText := strings.TrimSpace(r.msg.Options["amount"])
	destination := strings.TrimSpace(r.msg.Options["to"])
	memo := strings.TrimSpace(r.msg.Options["memo"])
	if amountText == "" || destination == "" {
		// Free text is the decoder's job, not this command's. A slash command
		// that guessed at "5k to the auditor" would be a second, worse decoder
		// with none of the re-checking the real one does.
		return r.reply(ctx, "Try: /pay amount:250 to:GABC…")
	}

	amount, err := money.Parse(amountText)
	if err != nil {
		return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount.", amountText))
	}
	if amount <= 0 {
		return r.reply(ctx, "A payment has to be for more than nothing.")
	}
	if !strkey.IsValidEd25519PublicKey(destination) {
		return r.reply(ctx,
			"That destination isn't a Stellar account. It needs to be a G… address.")
	}

	view, err := s.sender.ProposePayment(ctx, r.scope, api.ProposePaymentParams{
		Treasury:    treasury.ID,
		Destination: destination,
		Amount:      amount,
		Memo:        memo,
		CreatedBy:   r.member.ID,
	})
	switch {
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		// Not a queue-discipline preference. Two open envelopes from one
		// account reserve the same sequence number, so whichever executes first
		// silently kills the other.
		open, _, findErr := s.store.OpenProposal(ctx, r.org.ID, treasury.ID)
		if findErr != nil {
			return findErr
		}
		return r.reply(ctx, fmt.Sprintf(
			"%s already has proposal #%d open, and one account can only have one "+
				"at a time — both would reserve the same sequence number, and "+
				"whichever went first would silently kill the other.\n\n"+
				"Finish it with /approve %d and /execute %d, or cancel it.",
			treasuryName(treasury), open.ID, open.ID, open.ID))
	case err != nil:
		return err
	}

	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}

	// What the envelope does, read back out of the envelope, rather than what
	// was typed. The two should agree; saying the second would hide it on the
	// day they do not.
	what := "a payment"
	if view.Description != nil && len(view.Description.Operations) == 1 {
		what = view.Description.Operations[0].Summary
	}

	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d, from %s: %s\n\n"+
			"It needs signing weight %d and has %d. Sign it here:\n%s\n\n"+
			"Everyone else signing runs /approve %d to get their own link — this "+
			"one is yours. Whoever is last can submit it, or anyone can run "+
			"/execute %d once it has the weight.",
		view.Proposal.ID, treasuryName(treasury), what,
		view.Need, view.Have, link, view.Proposal.ID, view.Proposal.ID))
}

// approve hands one member their own link to sign a proposal.
func (s *Service) approve(ctx context.Context, r *request) error {
	view, err := s.namedProposal(ctx, r)
	if err != nil || view == nil {
		return err
	}
	if view.Proposal.Status != store.ProposalOpen {
		return r.reply(ctx, fmt.Sprintf(
			"Proposal #%d is %s. Nothing more to sign.", view.Proposal.ID, view.Proposal.Status))
	}

	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d has weight %d of %d.\n\n"+
			"Your link, valid for %d minutes:\n%s\n\n"+
			"The page shows what the envelope actually does, read from the "+
			"envelope rather than from what stelfin says about it.",
		view.Proposal.ID, view.Have, view.Need,
		int(approveLinkLifetime.Minutes()), link))
}

// execute submits a proposal that has reached its threshold.
func (s *Service) execute(ctx context.Context, r *request) error {
	view, err := s.namedProposal(ctx, r)
	if err != nil || view == nil {
		return err
	}

	result, err := s.sender.ExecuteProposal(ctx, r.scope, view.Proposal.ID, 0)
	switch {
	case errors.Is(err, api.ErrNotEnoughSignatures):
		return r.reply(ctx, fmt.Sprintf(
			"Proposal #%d has weight %d of %d. Still waiting on:\n%s",
			view.Proposal.ID, view.Have, view.Need, bulleted(view.Missing)))
	case errors.Is(err, api.ErrSequenceMoved):
		// The silent killer of multisig, said out loud. The envelope reserved
		// one sequence number and the account has moved past it, so no set of
		// signatures can ever make it valid.
		return r.reply(ctx, fmt.Sprintf(
			"Proposal #%d can't be submitted any more.\n\n"+
				"Something else spent from this treasury while it was collecting "+
				"signatures, so the envelope's sequence number is used up. Nothing "+
				"was paid twice and nothing is lost — the proposal has to be made "+
				"again and re-signed. Run /pay to start a fresh one.",
			view.Proposal.ID))
	case errors.Is(err, store.ErrProposalClosed):
		return r.reply(ctx, fmt.Sprintf("Proposal #%d is already closed.", view.Proposal.ID))
	case err != nil:
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d is on chain.\n\nTransaction %s, ledger %d.",
		view.Proposal.ID, result.Hash, result.Ledger))
}

// proposals lists what is open and what happened recently.
func (s *Service) proposals(ctx context.Context, r *request) error {
	list, err := s.store.Proposals(ctx, r.org.ID, 10)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "No proposals here yet. /pay starts one.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Recent proposals for %s:\n", r.org.DisplayName)
	for _, p := range list {
		fmt.Fprintf(&b, "\n#%d · %s · %s", p.ID, p.Kind, p.Status)
		if p.Status == store.ProposalOpen {
			fmt.Fprintf(&b, " · expires %s", p.ExpiresAt.UTC().Format("2 Jan 15:04 MST"))
		}
	}
	b.WriteString("\n\nRun /approve <number> to sign one.")
	return r.reply(ctx, b.String())
}

// namedProposal resolves the proposal a command refers to.
//
// With no number given, the treasury's one open proposal — because there can
// only be one, so naming it adds nothing but a chance to mistype it.
func (s *Service) namedProposal(ctx context.Context, r *request) (*api.ProposalView, error) {
	arg := strings.TrimSpace(r.msg.Options["id"])
	if arg == "" {
		arg = strings.TrimSpace(r.msg.Args)
	}

	var id store.ProposalID
	if arg == "" {
		treasury, err := s.soleTreasury(ctx, r)
		if err != nil || treasury.ID == 0 {
			return nil, err
		}
		open, ok, err := s.store.OpenProposal(ctx, r.org.ID, treasury.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, r.reply(ctx, "Nothing is open on that treasury. /pay starts a proposal.")
		}
		id = open.ID
	} else {
		var n int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(arg, "#"), "%d", &n); err != nil || n <= 0 {
			return nil, r.reply(ctx, fmt.Sprintf("I couldn't read %q as a proposal number.", arg))
		}
		id = store.ProposalID(n)
	}

	view, err := s.sender.LoadProposal(ctx, r.scope, id)
	if errors.Is(err, store.ErrNoProposal) {
		return nil, r.reply(ctx, fmt.Sprintf("There's no proposal #%d here.", id))
	}
	if err != nil {
		return nil, err
	}
	return view, nil
}

// soleTreasury picks the workspace's treasury, or explains why it cannot.
//
// A zero-valued Treasury with a nil error means a reply was already sent — the
// caller has nothing left to do.
func (s *Service) soleTreasury(ctx context.Context, r *request) (store.Treasury, error) {
	named := strings.TrimSpace(r.msg.Options["treasury"])

	list, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return store.Treasury{}, err
	}
	switch {
	case len(list) == 0:
		return store.Treasury{}, r.reply(ctx,
			"No treasury is linked here yet. An admin can run /link-treasury GABC… first.")
	case named == "":
		if len(list) > 1 {
			var b strings.Builder
			b.WriteString("This workspace has more than one treasury. Say which:\n")
			for _, t := range list {
				fmt.Fprintf(&b, "\n· treasury:%s", t.Address)
			}
			return store.Treasury{}, r.reply(ctx, b.String())
		}
		return list[0], nil
	}

	for _, t := range list {
		if t.Address == named || strings.EqualFold(t.Label, named) {
			return t, nil
		}
	}
	return store.Treasury{}, r.reply(ctx, fmt.Sprintf("I don't know a treasury called %q here.", named))
}

func treasuryName(t store.Treasury) string {
	if t.Label != "" {
		return t.Label
	}
	return t.Address
}

func bulleted(items []string) string {
	if len(items) == 0 {
		return "· nobody"
	}
	var b strings.Builder
	for i, item := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("· ")
		b.WriteString(item)
	}
	return b.String()
}

// proposalCommands are registered alongside the rest.
func (s *Service) proposalCommands() []*command {
	return []*command{
		{
			name:        "pay",
			description: "Propose a payment from the treasury",
			options: []chat.Option{
				{Name: "amount", Description: "How much", Type: chat.OptString, Required: true},
				{Name: "to", Description: "The G… account to pay", Type: chat.OptString, Required: true},
				{Name: "memo", Description: "A note that goes on chain", Type: chat.OptString},
				{Name: "treasury", Description: "Which treasury, if there are several", Type: chat.OptString},
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.pay(ctx, r) },
		},
		{
			name:        "approve",
			description: "Get your own link to sign an open proposal",
			options: []chat.Option{
				{Name: "id", Description: "The proposal number", Type: chat.OptString},
			},
			// Observer, not approver. This hands somebody a link; the account's
			// signer list decides whether their signature counts, and a
			// platform role standing in for that is the bug that loses a
			// treasury.
			minRole: chat.RoleObserver,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.approve(ctx, r) },
		},
		{
			name:        "execute",
			description: "Submit a proposal that has the signatures it needs",
			options: []chat.Option{
				{Name: "id", Description: "The proposal number", Type: chat.OptString},
			},
			// Deliberately open. Collecting the signatures is the
			// authorisation; requiring a particular person to press the button
			// would make this deployment a liveness dependency for somebody
			// else's money.
			minRole: chat.RoleObserver,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.execute(ctx, r) },
		},
		{
			name:        "proposals",
			description: "List recent proposals",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.proposals(ctx, r) },
		},
	}
}
