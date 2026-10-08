package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// contractCmd allows and removes third-party contracts.
//
// Allowing a contract records it and what code it runs now. It authorises no
// call: each call is a proposal the treasury's signers sign like any other. What
// the allowlist adds is the pin — a contract whose code has been replaced since
// it was allowed is refused, at proposal and again at execution, until somebody
// looks at the new code and allows it again.
func (s *Service) contractCmd(ctx context.Context, r *request) error {
	words := strings.Fields(r.msg.Args)
	word := func(i int) string {
		if i < len(words) {
			return words[i]
		}
		return ""
	}
	action := strings.ToLower(firstOf(r.msg.Options["action"], word(0)))

	switch action {
	case "allow":
		address := firstOf(r.msg.Options["address"], word(1))
		label := strings.ToLower(firstOf(r.msg.Options["label"], word(2)))
		if address == "" || label == "" {
			return r.reply(ctx, "Try: /contract allow C… usdc")
		}
		allowed, err := s.sender.AllowContract(ctx, r.scope, r.member.ID, address, label)
		if reply, handled := contractRefusal(label, err); handled {
			return r.reply(ctx, reply)
		}
		switch {
		case errors.Is(err, api.ErrConnectorName):
			return r.reply(ctx, "A label is lower-case letters, digits and hyphens, starting with a letter.")
		case errors.Is(err, store.ErrContractExists):
			return r.reply(ctx, "That label or that contract is already allowed here. /contracts lists them.")
		case err != nil:
			return err
		}
		s.log.Info("contract allowed", "org", r.org.ID, "label", allowed.Label,
			"address", allowed.Address, "executable", allowed.Executable, "by", r.member.ID)
		return r.reply(ctx, fmt.Sprintf(
			"Allowed %s as %q, running %s.\n\n"+
				"That authorises no call: each one is a proposal the treasury's signers sign. "+
				"If the contract's code is replaced, I'll refuse to call it until it is allowed again.",
			allowed.Address, allowed.Label, describeExecutable(allowed)))

	case "remove":
		label := strings.ToLower(firstOf(r.msg.Options["label"], word(1)))
		if label == "" {
			return r.reply(ctx, "Try: /contract remove usdc")
		}
		err := s.store.DisallowContract(ctx, r.org.ID, label)
		switch {
		case errors.Is(err, store.ErrContractNotAllowed):
			return r.reply(ctx, fmt.Sprintf("Nothing is allowed here as %q. /contracts lists what is.", label))
		case err != nil:
			return err
		}
		return r.reply(ctx, fmt.Sprintf("Removed %q. Proposals already open that call it will be refused at execution.", label))

	default:
		return r.reply(ctx, "Try: /contract allow C… usdc — or /contract remove usdc")
	}
}

func describeExecutable(c store.AllowedContract) string {
	if c.Executable == "wasm" {
		return "code " + c.WasmHash[:12] + "…"
	}
	return "the asset's own contract, which has no code that can be replaced"
}

// contractRefusal turns the refusals a contract command can meet into replies.
func contractRefusal(label string, err error) (string, bool) {
	switch {
	case errors.Is(err, api.ErrContractCallsOff):
		return "Calling contracts isn't enabled on this deployment. Whoever runs it decides that: " +
			"a contract call can move money in ways the spend limits here cannot see.", true
	case errors.Is(err, settlement.ErrNoSoroban):
		return "This deployment isn't connected to a Soroban RPC, so it can't read or call contracts.", true
	case errors.Is(err, settlement.ErrUnknownContract):
		return "There's no contract at that address on this network.", true
	case errors.Is(err, store.ErrContractNotAllowed):
		return fmt.Sprintf("Nothing is allowed here as %q. An admin can: /contract allow C… %s", label, label), true
	case errors.Is(err, api.ErrContractChanged):
		return fmt.Sprintf(
			"I won't call %q: its code has been replaced since it was allowed. That may be an "+
				"ordinary upgrade or it may not. Somebody should look at the new code; an admin can "+
				"then /contract remove %s and allow it again.", label, label), true
	case errors.Is(err, api.ErrBadArgument):
		return "I couldn't read the arguments: " + afterPrefix(err) +
			"\n\nEach is type:value — address:G…, i128:250000000, u32:7, bool:true, symbol:x, " +
			"string:\"two words\", bytes:00ff.", true
	}
	return "", false
}

// contracts lists what is allowed.
func (s *Service) contracts(ctx context.Context, r *request) error {
	list, err := s.store.AllowedContracts(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "No contracts are allowed here. An admin can: /contract allow C… label")
	}
	var b strings.Builder
	b.WriteString("Contracts this workspace may call:\n")
	for _, c := range list {
		fmt.Fprintf(&b, "\n· %s — %s — %s", c.Label, c.Address, describeExecutable(c))
	}
	return r.reply(ctx, b.String())
}

// callArguments reads a contract, a function and its arguments.
func callArguments(m chat.Inbound) (label, function, arguments string) {
	label = strings.ToLower(strings.TrimSpace(m.Options["contract"]))
	function = strings.TrimSpace(m.Options["function"])
	arguments = strings.TrimSpace(m.Options["arguments"])
	if label == "" {
		first, rest, _ := strings.Cut(strings.TrimSpace(m.Args), " ")
		second, after, _ := strings.Cut(strings.TrimSpace(rest), " ")
		label, function, arguments = strings.ToLower(first), second, strings.TrimSpace(after)
	}
	return label, function, arguments
}

// restore proposes bringing back the archived state a call needs.
func (s *Service) restore(ctx context.Context, r *request) error {
	label, function, arguments := callArguments(r.msg)
	if label == "" || function == "" {
		return r.reply(ctx, "Try: /restore usdc transfer address:G… address:G… i128:50000000 — the call that needs it.")
	}
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}
	view, err := s.sender.ProposeRestore(ctx, r.scope, treasury.ID, label, function, arguments, r.member.ID)
	if reply, handled := contractRefusal(label, err); handled {
		return r.reply(ctx, reply)
	}
	switch {
	case errors.Is(err, api.ErrNothingArchived):
		return r.reply(ctx, "That call needs nothing restored. /invoke it directly.")
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		return r.reply(ctx, fmt.Sprintf("%s already has a proposal open. Finish that one first.", treasuryName(treasury)))
	case err != nil:
		return err
	}
	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}
	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d, from %s: restore the archived state that call needs.\n\n"+
			"It changes nothing but what can be read; the page lists every entry and what it costs. "+
			"It needs signing weight %d and has %d:\n%s",
		view.Proposal.ID, treasuryName(treasury), view.Need, view.Have, link))
}

// invoke proposes a call to an allowed contract from the treasury.
func (s *Service) invoke(ctx context.Context, r *request) error {
	label, function, arguments := callArguments(r.msg)
	if label == "" || function == "" {
		return r.reply(ctx, "Try: /invoke usdc transfer address:G… address:G… i128:50000000")
	}
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}

	view, err := s.sender.ProposeInvoke(ctx, r.scope, treasury.ID, label, function, arguments, r.member.ID)
	if reply, handled := contractRefusal(label, err); handled {
		return r.reply(ctx, reply)
	}
	switch {
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		return r.reply(ctx, fmt.Sprintf(
			"%s already has a proposal open, and one account can only have one at a time. Finish that one first.",
			treasuryName(treasury)))
	case errors.Is(err, settlement.ErrSimulationFailed):
		return r.reply(ctx, "The network simulated that call and it would fail, so I haven't put it to the signers. "+
			"Check the function and the arguments.")
	case errors.Is(err, settlement.ErrArchivedState):
		return r.reply(ctx, fmt.Sprintf(
			"That call needs contract state that has been archived to be restored first. "+
				"Restoring costs money and is a transaction of its own, so I won't fold it in. "+
				"To put the restore to the signers: /restore %s %s %s — then /invoke again once it has run.",
			label, function, arguments))
	case err != nil:
		return err
	}

	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}
	what := "a contract call"
	if view.Description != nil && len(view.Description.Operations) == 1 {
		what = view.Description.Operations[0].Summary
	}
	return r.reply(ctx, fmt.Sprintf(
		"Proposal #%d, from %s: %s\n\n"+
			"It needs signing weight %d and has %d. Sign it here:\n%s\n\n"+
			"The page shows the call as read from the envelope. A contract can do anything its "+
			"code says, so read it as you would the code: the limits on payments here don't "+
			"apply to what a contract does.",
		view.Proposal.ID, treasuryName(treasury), what, view.Need, view.Have, link))
}

// contractCommands are registered alongside the rest.
func (s *Service) contractCommands() []*command {
	opt := func(name, description string, required bool) chat.Option {
		return chat.Option{Name: name, Description: description, Type: chat.OptString, Required: required}
	}
	return []*command{
		{
			name:        "contract",
			needs:       needsContractCalls,
			description: "Allow or remove a third-party contract",
			options: []chat.Option{
				opt("action", "allow or remove", true),
				opt("address", "When allowing: the C… address", false),
				opt("label", "What this workspace calls it", false),
			},
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.contractCmd(ctx, r) },
		},
		{
			name:        "contracts",
			needs:       needsContractCalls,
			description: "List the contracts this workspace may call",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.contracts(ctx, r) },
		},
		{
			name:        "restore",
			needs:       needsContractCalls,
			description: "Propose restoring archived state a contract call needs",
			options: []chat.Option{
				opt("contract", "Its label here", true),
				opt("function", "The call that needs it", true),
				opt("arguments", "type:value, separated by spaces", false),
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.restore(ctx, r) },
		},
		{
			name:        "invoke",
			needs:       needsContractCalls,
			description: "Propose a call to an allowed contract from the treasury",
			options: []chat.Option{
				opt("contract", "Its label here", true),
				opt("function", "The function to call", true),
				opt("arguments", "type:value, separated by spaces", false),
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.invoke(ctx, r) },
		},
	}
}
