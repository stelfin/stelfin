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
	if treasury.Kind == store.TreasuryContract {
		return s.payFromContract(ctx, r, treasury)
	}

	args := payArguments(r.msg)
	amountText, destination, memo := args["amount"], args["to"], args["memo"]
	if amountText == "" || destination == "" {
		// Free text is the decoder's job, not this command's. A slash command
		// that guessed at "5k to the auditor" would be a second, worse decoder
		// with none of the re-checking the real one does.
		return r.reply(ctx, "Try: /pay 250 to GABC…")
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
	var over *api.CeilingError
	var notReady *api.DestinationError
	switch {
	case errors.As(err, &over):
		return r.reply(ctx, overCeilingReply(over))
	case errors.As(err, &notReady):
		return r.reply(ctx, fmt.Sprintf(
			"I haven't put that to the signers, because it wouldn't land: %s %s.\n\n"+
				"Better to find that out now than after everyone has signed. Once the "+
				"destination is ready, run /pay again.", notReady.Address, notReady.Reason))
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		// Not a queue-discipline preference. Two open envelopes from one
		// account reserve the same sequence number, so whichever executes first
		// silently kills the other.
		open, found, findErr := s.store.OpenProposal(ctx, r.org.ID, treasury.ID)
		if findErr != nil {
			return findErr
		}
		if !found {
			// The store gives one answer for two collisions, and this is the
			// other one: nothing is open, and this exact envelope has been
			// proposed before. Naming "proposal #0" would send somebody
			// looking for a proposal that does not exist.
			return r.reply(ctx,
				"That exact proposal has already been made here and is closed now. "+
					"Run /pay again in a moment and it will be a fresh one.")
		}
		return r.reply(ctx, fmt.Sprintf(
			"%s already has proposal #%d open, and one account can only have one "+
				"at a time — both would reserve the same sequence number, and "+
				"whichever went first would silently kill the other.\n\n"+
				"Finish it with /approve %d and /execute %d, or withdraw it with /cancel %d.",
			treasuryName(treasury), open.ID, open.ID, open.ID, open.ID))
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

// overCeilingReply says which limit a proposal ran into and who can change it.
func overCeilingReply(e *api.CeilingError) string {
	what := fmt.Sprintf("more than %s in one proposal", e.Limit.Display())
	if e.Window == "day" {
		what = fmt.Sprintf("more than %s in 24 hours (this would make %s)", e.Limit.Display(), e.Asked.Display())
	}
	if e.Scope == "workspace" {
		return fmt.Sprintf(
			"I haven't put that to the signers: this workspace has said not to propose %s.\n\n"+
				"An admin can change that with /limits.", what)
	}
	return fmt.Sprintf(
		"I haven't put that to the signers: this deployment won't propose %s, "+
			"whatever a workspace's own limits are. That's set by whoever runs it.", what)
}

// byKind sends a command about a numbered proposal down the road for the
// treasury it is about. done is true when there is nothing left for the
// caller: it was a contract treasury's, and has been dealt with, or the
// question of which treasury has been put back to whoever asked.
func (s *Service) byKind(
	ctx context.Context, r *request,
	onChain func(treasury store.Treasury, number string) error,
) (done bool, err error) {
	args, rest := named(r.msg, "id", "treasury")
	treasury, kind, err := s.addressed(ctx, r, args["treasury"])
	switch kind {
	case replied:
		return true, err
	case contractTreasury:
		number := args["id"]
		if number == "" && len(rest) > 0 {
			number = rest[0]
		}
		return true, onChain(treasury, number)
	}
	// A classic treasury, read as it always was — less the name of the
	// treasury, if one was given, which that reading does not expect.
	r.msg.Args = strings.Join(rest, " ")
	return false, nil
}

// approve hands one member their own link to sign a proposal.
func (s *Service) approve(ctx context.Context, r *request) error {
	if done, err := s.byKind(ctx, r, func(t store.Treasury, number string) error {
		return s.vote(ctx, r, t, number, true)
	}); done || err != nil {
		return err
	}
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
	if done, err := s.byKind(ctx, r, func(t store.Treasury, number string) error {
		return s.executeOnChain(ctx, r, t, number)
	}); done || err != nil {
		return err
	}
	view, err := s.namedProposal(ctx, r)
	if err != nil || view == nil {
		return err
	}

	result, err := s.sender.ExecuteProposal(ctx, r.scope, view.Proposal.ID, 0)
	var refused *api.RefusedError
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
	case errors.As(err, &refused):
		return r.reply(ctx, refusedReply(view.Proposal.ID, refused))
	case err != nil:
		return err
	}

	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d is on chain.\n\nTransaction %s, ledger %d.",
		view.Proposal.ID, result.Hash, result.Ledger))
}

// refusedReply says what the network objected to and what that leaves.
//
// The two halves matter equally. The reason, because it is the only thing
// anybody can act on. And whether the proposal is still open, because the
// answer decides who does what next: an open one is fixed and executed again
// with the signatures it has, a closed one is proposed again and signed again.
func refusedReply(id store.ProposalID, e *api.RefusedError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The network refused proposal #%d: %s.\n\n", id, refusalReason(e))
	if e.Closed {
		fmt.Fprintf(&b, "That used up the envelope, so #%d is closed — its signatures cannot be "+
			"reused. Nothing was paid. Once the cause is dealt with, propose it again.", id)
	} else {
		fmt.Fprintf(&b, "Nothing was spent and #%d is still open with the signatures it has. "+
			"Once that is put right, run /execute %d again.", id, id)
	}
	return b.String()
}

// refusalReason puts the network's result codes into words.
//
// Only the ones a treasury meets in ordinary use. Anything else is given as the
// code itself: a code somebody can look up is worth more than a sentence that
// guesses.
func refusalReason(e *api.RefusedError) string {
	for _, op := range e.Operations {
		switch op {
		case "op_underfunded":
			return "the treasury does not hold enough to pay it"
		case "op_no_destination":
			return "the destination account does not exist on the network"
		case "op_no_trust":
			return "the destination has no trustline for the asset, so it cannot receive it"
		case "op_src_no_trust":
			return "the treasury has no trustline for the asset"
		case "op_line_full":
			return "the destination's trustline is full and cannot hold that much more"
		case "op_not_authorized", "op_src_not_authorized":
			return "the asset's issuer has not authorised one of the accounts to hold it"
		case "op_low_reserve":
			return "it would leave an account below the XLM reserve the network requires"
		case "op_under_dest_min", "op_too_few_offers":
			return "the market moved, and the swap could no longer deliver the minimum it promised"
		case "op_success", "op_inner":
			continue
		default:
			return "an operation failed with " + op
		}
	}
	switch e.Transaction {
	case "tx_bad_auth", "tx_bad_auth_extra":
		return "the signatures on it are not what the treasury now requires — " +
			"its signers or thresholds have changed since it was signed. " +
			"An admin can run /link-treasury again so I have the current signer set"
	case "tx_insufficient_balance", "tx_insufficient_fee":
		return "the treasury cannot cover the transaction fee, which it pays itself in XLM"
	case "tx_too_late":
		return "its signing window had already closed"
	case "tx_bad_seq":
		return "something else spent from this treasury first, so its sequence number is used up"
	case "tx_no_source_account":
		return "the treasury account does not exist on the network"
	}
	return "the result code was " + e.Transaction
}

// cancel withdraws an open proposal.
func (s *Service) cancel(ctx context.Context, r *request) error {
	if done, err := s.byKind(ctx, r, func(t store.Treasury, number string) error {
		return s.withdraw(ctx, r, t, number)
	}); done || err != nil {
		return err
	}
	view, err := s.namedProposal(ctx, r)
	if err != nil || view == nil {
		return err
	}

	proposal, err := s.sender.CancelProposal(ctx, r.scope, view.Proposal.ID,
		r.member.ID, r.role.AtLeast(chat.RoleAdmin))
	switch {
	case errors.Is(err, store.ErrProposalClosed):
		return r.reply(ctx, fmt.Sprintf(
			"Proposal #%d is already %s. There is nothing left to withdraw.",
			proposal.ID, proposal.Status))
	case errors.Is(err, api.ErrNotYoursToCancel):
		return r.reply(ctx, fmt.Sprintf(
			"Proposal #%d was made by member #%d. They can withdraw it, and so can an admin here.",
			proposal.ID, proposal.CreatedBy))
	case err != nil:
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Proposal #%d is withdrawn, and %s is free for a new one.",
		proposal.ID, treasuryName(view.Treasury))
	if view.Have > 0 {
		// Not a formality. A signature is over a transaction, not over a row
		// here, and nothing this deployment does can take one back.
		fmt.Fprintf(&b, "\n\nIt had already been signed, and those signatures stay valid on "+
			"the network until %s or until the treasury next transacts, whichever comes first. "+
			"I won't offer the envelope to anyone else, but a signer who kept a copy could still "+
			"add theirs. If that matters, send any transaction from the treasury — a new proposal "+
			"that executes will do — and this one can never land.",
			proposal.ExpiresAt.UTC().Format("2 Jan 15:04 MST"))
	}
	return r.reply(ctx, b.String())
}

// proposals lists what is open and what happened recently.
func (s *Service) proposals(ctx context.Context, r *request) error {
	// So that nothing past its window is listed as open. The timer that does
	// this cannot be relied on to have run since the window closed.
	if _, err := s.store.ExpireProposals(ctx); err != nil {
		return err
	}
	list, err := s.store.Proposals(ctx, r.org.ID, 10)
	if err != nil {
		return err
	}
	treasuries, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return err
	}
	var held []store.Treasury
	for _, t := range treasuries {
		if t.Kind == store.TreasuryContract {
			held = append(held, t)
		}
	}
	if len(list) == 0 && len(held) == 0 {
		return r.reply(ctx, "No proposals here yet. /pay starts one.")
	}

	var b strings.Builder
	if len(list) > 0 {
		fmt.Fprintf(&b, "Recent proposals for %s:\n", r.org.DisplayName)
		for _, p := range list {
			fmt.Fprintf(&b, "\n#%d · %s · %s", p.ID, p.Kind, p.Status)
			if p.Status == store.ProposalOpen {
				fmt.Fprintf(&b, " · expires %s", p.ExpiresAt.UTC().Format("2 Jan 15:04 MST"))
			}
		}
		b.WriteString("\n\nRun /approve <number> to sign one, or /cancel <number> to withdraw one that is open.")
	}
	// A treasury held by a contract keeps its own proposals and numbers them
	// itself, so each is listed apart, read from the contract.
	for _, t := range held {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if err := s.chainProposals(ctx, r, t, &b); err != nil {
			if handled, failed := s.refusedOnChain(ctx, r, t, err); handled || failed != nil {
				return failed
			}
		}
	}
	return r.reply(ctx, strings.TrimRight(b.String(), "\n"))
}

// payArguments reads /pay's arguments from wherever the platform put them.
//
// Discord delivers them as structured options. Telegram has no such thing: a
// command is a word followed by text, so there the same four values have to be
// read out of Args, or /pay is a command that can only ever answer with its own
// usage line.
//
// The text form is deliberately rigid — an amount, an optional "to", an
// address, then a memo — with "name:value" accepted for any of them. It is a
// fixed grammar and not a guess: anything looser is the decoder's job, and the
// decoder's output gets re-checked against what was typed where this does not.
func payArguments(m chat.Inbound) map[string]string {
	out := make(map[string]string, 4)
	for _, key := range []string{"amount", "to", "memo", "treasury"} {
		if v := strings.TrimSpace(m.Options[key]); v != "" {
			out[key] = v
		}
	}
	if len(out) > 0 {
		return out
	}

	words := strings.Fields(m.Args)
	for i := 0; i < len(words); i++ {
		w := words[i]
		if key, value, ok := strings.Cut(w, ":"); ok {
			switch key = strings.ToLower(key); key {
			case "memo":
				// A memo is prose, so it runs to the end of the line.
				rest := append([]string{value}, words[i+1:]...)
				out["memo"] = strings.TrimSpace(strings.Join(rest, " "))
				return out
			case "amount", "to", "treasury":
				out[key] = value
				continue
			}
		}
		switch {
		case out["amount"] == "":
			out["amount"] = w
		case out["to"] == "":
			if strings.EqualFold(w, "to") {
				continue
			}
			out["to"] = w
		default:
			out["memo"] = strings.Join(words[i:], " ")
			return out
		}
	}
	return out
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
	if named == "" && r.msg.Command == "pay" {
		named = payArguments(r.msg)["treasury"]
	}

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
			name:        "cancel",
			description: "Withdraw an open proposal",
			options: []chat.Option{
				{Name: "id", Description: "The proposal number", Type: chat.OptString},
			},
			// Whoever may propose may take back what they proposed. Whether
			// this particular proposal is theirs to take back is checked
			// against the proposal itself, not against a role.
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.cancel(ctx, r) },
		},
		{
			name:        "proposals",
			description: "List recent proposals",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.proposals(ctx, r) },
		},
	}
}
