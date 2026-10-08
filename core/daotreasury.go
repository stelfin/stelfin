package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/contracts/treasuryabi"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// A treasury held by a contract, from chat.
//
// The other kind of treasury is an account several people sign for. This kind
// is a contract its members vote in, and nearly every command here ends the
// same way: with a link, for the one person who typed it, to a page where they
// sign the one call they asked for. Nobody else can use that link and nothing
// happens until they do.
//
// Nothing in this file decides anything about a treasury. Who its members are,
// whether a proposal passed and what it may pay are the contract's to say, and
// are read from it each time. A role here still only decides who the bot will
// talk to — a member with every role and no vote can propose nothing, and the
// contract is what tells them so.

// Default rules for a new treasury. A workspace that wants others says so when
// it creates one, and changes them afterwards by vote.
const (
	defaultQuorumBps   = 5_000
	defaultApprovalBps = 6_000
	defaultVoting      = 3 * 24 * time.Hour
	// A day. The contract allows none, and treats that as a decision a
	// treasury makes about itself; this deployment does not make it for them.
	defaultTimelock = 24 * time.Hour
)

// treasuryKind is which sort of treasury a command is addressed to.
type treasuryKind int

const (
	// replied means the question was answered already: an unknown name, or
	// several treasuries and none named. The caller has nothing left to do.
	replied treasuryKind = iota
	classicTreasury
	contractTreasury
)

// named reads a command's arguments from wherever the platform put them.
//
// Discord delivers them as options. Telegram delivers a line of text, in which
// `name:value` is accepted for any of the keys given and everything else is
// returned in order.
func named(m chat.Inbound, keys ...string) (values map[string]string, rest []string) {
	values = make(map[string]string, len(keys))
	known := make(map[string]bool, len(keys))
	for _, key := range keys {
		known[key] = true
		if v := strings.TrimSpace(m.Options[key]); v != "" {
			values[key] = v
		}
	}
	for _, word := range strings.Fields(m.Args) {
		if key, value, ok := strings.Cut(word, ":"); ok && known[strings.ToLower(key)] {
			if _, taken := values[strings.ToLower(key)]; !taken {
				values[strings.ToLower(key)] = value
			}
			continue
		}
		rest = append(rest, word)
	}
	return values, rest
}

// addressed works out which treasury a command about a proposal is for, and
// which sort it is.
//
// A workspace with only classic treasuries is routed exactly as it always was.
// The question only arises once one is held by a contract, because then
// "proposal 3" names something different in each: a contract numbers its own
// proposals from one.
func (s *Service) addressed(ctx context.Context, r *request, name string) (store.Treasury, treasuryKind, error) {
	list, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return store.Treasury{}, replied, err
	}
	contracts := 0
	for _, t := range list {
		if t.Kind == store.TreasuryContract {
			contracts++
		}
	}
	if contracts == 0 {
		return store.Treasury{}, classicTreasury, nil
	}

	if name != "" {
		for _, t := range list {
			if t.Address == name || strings.EqualFold(t.Label, name) {
				if t.Kind == store.TreasuryContract {
					return t, contractTreasury, nil
				}
				return t, classicTreasury, nil
			}
		}
		return store.Treasury{}, replied, r.reply(ctx,
			fmt.Sprintf("I don't know a treasury called %q here. /treasuries lists them.", name))
	}
	if len(list) == 1 {
		return list[0], contractTreasury, nil
	}

	var b strings.Builder
	b.WriteString("This workspace has more than one treasury, and each numbers its own proposals. Say which:\n")
	for _, t := range list {
		fmt.Fprintf(&b, "\n· treasury:%s", treasuryName(t))
	}
	return store.Treasury{}, replied, r.reply(ctx, b.String())
}

// onlyContract picks the contract treasury a command is for, when the command
// means nothing for any other kind.
func (s *Service) onlyContract(ctx context.Context, r *request, name, command string) (store.Treasury, bool, error) {
	list, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return store.Treasury{}, false, err
	}
	var held []store.Treasury
	for _, t := range list {
		if t.Kind == store.TreasuryContract {
			held = append(held, t)
		}
	}

	if name != "" {
		for _, t := range list {
			if t.Address != name && !strings.EqualFold(t.Label, name) {
				continue
			}
			if t.Kind != store.TreasuryContract {
				return store.Treasury{}, false, r.reply(ctx, fmt.Sprintf(
					"%s is an account its signers sign for, and /%s is for a treasury held by a contract. "+
						"Its rules are its signers and thresholds, which are changed on the network.",
					treasuryName(t), command))
			}
			return t, true, nil
		}
		return store.Treasury{}, false, r.reply(ctx,
			fmt.Sprintf("I don't know a treasury called %q here. /treasuries lists them.", name))
	}

	switch len(held) {
	case 0:
		return store.Treasury{}, false, r.reply(ctx,
			"/"+command+" is for a treasury held by a contract, and this workspace has none. "+
				"An admin can make one with /create-treasury.")
	case 1:
		return held[0], true, nil
	}
	var b strings.Builder
	b.WriteString("This workspace has more than one treasury held by a contract. Say which:\n")
	for _, t := range held {
		fmt.Fprintf(&b, "\n· treasury:%s", treasuryName(t))
	}
	return store.Treasury{}, false, r.reply(ctx, b.String())
}

// needsWallet answers a member who has no address to call a contract from.
func (s *Service) needsWallet(ctx context.Context, r *request) error {
	return r.reply(ctx,
		"You need a wallet here first: a contract counts votes by address, and you have none in "+
			"this workspace yet. /link one you already have.")
}

// callLink hands a member the link to the one call they asked for.
func (s *Service) callLink(ctx context.Context, r *request, prepared *api.PreparedCall, lead, after string) error {
	link, err := r.links.IssueCallLink(r.scope, prepared.Hash, prepared.ExpiresAt)
	if err != nil {
		return err
	}
	minutes := int(time.Until(prepared.ExpiresAt).Round(time.Minute).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	return r.reply(ctx, fmt.Sprintf(
		"%s\n\nSign it here, within %d minutes:\n%s\n\n"+
			"The page reads the call back out of the transaction itself and says what it does. "+
			"It costs you nothing: stelfin pays the fee.%s",
		lead, minutes, link, after))
}

// refusedOnChain turns what stopped a call into something to say. handled is
// false when err is nothing this can explain.
func (s *Service) refusedOnChain(
	ctx context.Context, r *request, treasury store.Treasury, err error,
) (handled bool, failed error) {
	var (
		refusal *api.ContractRefusal
		token   *api.TokenRefusal
		said    *api.RefusalError
	)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, api.ErrNoWallet):
		return true, s.needsWallet(ctx, r)
	case errors.Is(err, api.ErrWalletNotOnChain):
		return true, r.reply(ctx, fmt.Sprintf(
			"Your wallet %s is not on the network yet: it has an address, and no account until "+
				"somebody sends it some XLM. A call has to come from an account.", r.member.Address))
	case errors.Is(err, api.ErrContractTreasuriesOff):
		return true, r.reply(ctx,
			"This deployment can't work with a treasury held by a contract: it has no Soroban RPC "+
				"configured, or no treasury code on this network.")
	case errors.Is(err, api.ErrNotTreasuryCode):
		return true, r.reply(ctx,
			"That contract isn't running the treasury code this deployment was built with, so "+
				"nothing I could say about it would be true. I haven't added it.")
	case errors.Is(err, api.ErrCallFailedOnChain):
		return true, r.reply(ctx,
			"That reached the network and the contract refused it there, which means something "+
				"changed between my check and the ledger. Nothing was paid. /proposals shows where things stand.")
	case errors.As(err, &said):
		return true, r.reply(ctx, sentence(said.Reason))
	case errors.As(err, &token):
		return true, r.reply(ctx,
			"The token wouldn't make that transfer. Most often that means your wallet doesn't hold "+
				"enough of it, or has no trustline for it. /balance shows what it holds.")
	case errors.As(err, &refusal):
		return true, r.reply(ctx, s.refusalText(refusal.Code, treasury, r.member))
	}
	return false, err
}

// sentence makes a reason somebody else wrote read as a reply.
func sentence(reason string) string {
	if reason == "" {
		return "I couldn't do that."
	}
	out := strings.ToUpper(reason[:1]) + reason[1:]
	if !strings.HasSuffix(out, ".") && !strings.HasSuffix(out, "…") {
		out += "."
	}
	return out
}

// refusalText is what each of the contract's refusals means to the person who
// ran into it.
func (s *Service) refusalText(code treasuryabi.Code, treasury store.Treasury, member store.Member) string {
	name := treasuryName(treasury)
	switch code {
	case treasuryabi.NotMember:
		return fmt.Sprintf(
			"Your wallet %s has no vote in %s, so the contract won't take that from you. "+
				"Its voters are its to change: /voter proposes one, and /treasuries lists who they are.",
			member.Address, name)
	case treasuryabi.UnknownProposal:
		return fmt.Sprintf("%s has no proposal with that number. /proposals lists them.", name)
	case treasuryabi.NotOpen:
		return "That proposal isn't open any more. /proposals shows what became of it."
	case treasuryabi.AlreadyVoted:
		return "You've already voted on that one. A vote is cast once."
	case treasuryabi.NotExecutable:
		return "That proposal can't be carried out yet: it hasn't passed, or its timelock is still running."
	case treasuryabi.RecipientNotAllowed:
		return fmt.Sprintf(
			"That address isn't one %s may pay. Its members decide who is: /allow GABC… proposes adding one.",
			name)
	case treasuryabi.SpendLimitExceeded:
		return fmt.Sprintf(
			"That is more than %s lets one member spend without a proposal. /rules shows the "+
				"allowance and what is left of it, and /pay puts a payment to the members instead.", name)
	case treasuryabi.InvalidPolicy:
		return "The contract won't take those rules. Quorum is 1 to 100 per cent, approval is more " +
			"than 50 and at most 100, voting has to stay open for some time, no period may be longer " +
			"than a year, and a spending allowance needs a window."
	case treasuryabi.InvalidAmount:
		return "A payment has to be for more than nothing."
	case treasuryabi.WouldLockOut:
		return "The contract won't take that: it would leave the treasury with nobody able to pass anything."
	case treasuryabi.NotProposer:
		return "Only whoever made that proposal can withdraw it. Everyone else votes against it, " +
			"or — once it has passed — proposes stopping it with /stop."
	case treasuryabi.InvalidMembers:
		return "The contract won't start with those voters: it needs at least one, each with some weight, each listed once."
	case treasuryabi.NothingToRevoke:
		return "There is nothing left to stop: that proposal has already been carried out, rejected or withdrawn."
	case treasuryabi.Undecided:
		return "That proposal is still being voted on, and the votes not yet cast could change it."
	case treasuryabi.TransferFailed:
		return fmt.Sprintf(
			"%s couldn't make that payment — most likely it doesn't hold enough. /balance shows "+
				"what it holds, and /deposit puts more in.", name)
	default:
		return fmt.Sprintf("The contract refused that, with a code I don't have words for (%d).", code)
	}
}

// createTreasury makes a treasury contract for this workspace.
func (s *Service) createTreasury(ctx context.Context, r *request) error {
	args, rest := named(r.msg, "name", "voting", "timelock", "quorum", "approval", "recipients")
	label := args["name"]
	if label == "" && len(rest) > 0 {
		label = rest[0]
	}
	if label == "" {
		return r.reply(ctx,
			"What should it be called? Try: /create-treasury ops\n\n"+
				"That makes a treasury held by a contract. Its voters start as everyone here with the "+
				"approver role and a wallet, one vote each. It opens voting for 3 days, needs half of "+
				"them to vote and 60% of those to agree, and waits a day before anything that passed "+
				"is carried out. Say otherwise with voting:2d timelock:12h quorum:50 approval:60, and "+
				"name anyone it should be able to pay from the start with recipients:GABC…,GDEF…")
	}
	if !labelPattern(label) {
		return r.reply(ctx, "A treasury's name is a short word: letters, digits and hyphens, starting with a letter.")
	}

	policy := treasuryabi.Policy{
		QuorumBps:    defaultQuorumBps,
		ApprovalBps:  defaultApprovalBps,
		VotingPeriod: uint64(defaultVoting.Seconds()),
		Timelock:     uint64(defaultTimelock.Seconds()),
		SpendLimit:   big.NewInt(0),
	}
	if reply := applyRules(&policy, args); reply != "" {
		return r.reply(ctx, reply)
	}

	var recipients []string
	for _, address := range strings.FieldsFunc(args["recipients"], func(c rune) bool { return c == ',' || c == ';' }) {
		address = strings.TrimSpace(address)
		if !payable(address) {
			return r.reply(ctx, fmt.Sprintf("%q isn't somewhere a payment can go. Recipients are G… or C… addresses.", address))
		}
		recipients = append(recipients, address)
	}

	existing, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return err
	}
	for _, t := range existing {
		if strings.EqualFold(t.Label, label) {
			return r.reply(ctx, fmt.Sprintf("There is already a treasury called %s here.", t.Label))
		}
	}

	// Everyone here who may approve and has an address to vote from. Read from
	// the roles granted by name: a role a platform confers depends on who is
	// speaking at the moment, and is not something to found a treasury on.
	everyone, err := s.store.Members(ctx, r.org.ID, 500)
	if err != nil {
		return err
	}
	var (
		voters  []treasuryabi.Member
		without int
	)
	for _, m := range everyone {
		if !m.Role.AtLeast(chat.RoleApprover) {
			continue
		}
		if !m.Member.HasAddress() {
			without++
			continue
		}
		voters = append(voters, treasuryabi.Member{Address: m.Member.Address, Weight: 1})
	}
	if len(voters) == 0 {
		return r.reply(ctx,
			"Nobody here could vote in it yet. A voter is somebody with the approver role and a "+
				"wallet: /grant gives the role, and each person runs /link for the wallet.")
	}

	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}
	state, err := s.sender.CreateContractTreasury(ctx, r.scope, api.CreateTreasuryParams{
		Label: label, Policy: policy, Members: voters, Recipients: recipients, By: identity,
	})
	if handled, failed := s.refusedOnChain(ctx, r, store.Treasury{Label: label}, err); handled || failed != nil {
		return failed
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s is created, and held by a contract at\n%s\n\n", label, state.Treasury.Address)
	b.WriteString(describeState(state))
	if without > 0 {
		fmt.Fprintf(&b, "\n\n%d approver(s) here had no wallet and were left out. /voter proposes adding one later.", without)
	}
	b.WriteString("\n\nNobody stands above its voters, stelfin included: every payment and every " +
		"change to what is above is a proposal they vote on. /deposit puts money in.")
	if len(state.Recipients) == 0 {
		b.WriteString(" It can pay nobody yet — /allow GABC… proposes the first recipient.")
	}
	return r.reply(ctx, b.String())
}

// adoptTreasury records a treasury contract that already exists.
func (s *Service) adoptTreasury(ctx context.Context, r *request, address string) error {
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	args, _ := named(r.msg, "name")
	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}
	state, err := s.sender.AdoptContractTreasury(ctx, r.scope, address, args["name"], r.member, identity)
	var refusal *api.ContractRefusal
	if errors.As(err, &refusal) && refusal.Code == treasuryabi.NotMember {
		return r.reply(ctx, fmt.Sprintf(
			"Your wallet %s has no vote in that treasury, by its own list. Somebody who does "+
				"has to be the one to add it here.", r.member.Address))
	}
	if handled, failed := s.refusedOnChain(ctx, r, store.Treasury{Address: address}, err); handled || failed != nil {
		return failed
	}
	return r.reply(ctx, fmt.Sprintf(
		"Added. %s is a treasury held by a contract, running the code this deployment was built "+
			"with, and you have a vote in it.\n\n%s", address, describeState(state)))
}

// describeState writes a contract treasury's rules, voters and recipients.
func describeState(state *api.TreasuryState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rules: %s.\n", api.DescribePolicy(state.Policy))
	fmt.Fprintf(&b, "Voters (%d, total weight %d):", len(state.Members), state.TotalWeight)
	for _, m := range state.Members {
		fmt.Fprintf(&b, "\n· %s — weight %d", m.Address, m.Weight)
	}
	if len(state.Recipients) == 0 {
		b.WriteString("\nMay pay: nobody yet.")
	} else {
		b.WriteString("\nMay pay:")
		for _, to := range state.Recipients {
			fmt.Fprintf(&b, "\n· %s", to)
		}
	}
	return b.String()
}

func labelPattern(label string) bool {
	if label == "" || len(label) > 31 {
		return false
	}
	for i, c := range label {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func payable(address string) bool {
	return strkey.IsValidEd25519PublicKey(address) || strkey.IsValidContractAddress(address)
}

// applyRules writes the rules somebody named over a policy. It returns what to
// tell them when one cannot be read, and nothing when all could.
func applyRules(p *treasuryabi.Policy, args map[string]string) string {
	for key, into := range map[string]*uint32{"quorum": &p.QuorumBps, "approval": &p.ApprovalBps} {
		text, ok := args[key]
		if !ok {
			continue
		}
		bps, ok := share(text)
		if !ok {
			return fmt.Sprintf("I couldn't read %q as a percentage for %s. Try %s:60", text, key, key)
		}
		*into = bps
	}
	for key, into := range map[string]*uint64{"voting": &p.VotingPeriod, "timelock": &p.Timelock, "window": &p.SpendWindow} {
		text, ok := args[key]
		if !ok {
			continue
		}
		seconds, err := span(text)
		if err != nil {
			return fmt.Sprintf("I couldn't read %q as a length of time for %s. Try %s:2d, or 12h, or 30m.", text, key, key)
		}
		*into = seconds
	}
	if text, ok := args["limit"]; ok {
		amount, err := money.Parse(text)
		if err != nil || amount < 0 {
			return fmt.Sprintf("I couldn't read %q as an amount for the spending allowance.", text)
		}
		p.SpendLimit = big.NewInt(int64(amount))
	}
	return ""
}

// share reads a share of the whole written as a percentage — "60", "66.67%" —
// into basis points. More than nothing and at most all of it, to two places.
//
// Read as digits and never as a float: these become the thresholds a treasury
// votes by, and 66.67 is not a number a float holds.
func share(text string) (uint32, bool) {
	text = strings.TrimSuffix(strings.TrimSpace(text), "%")
	whole, fraction, _ := strings.Cut(text, ".")
	if whole == "" || len(whole) > 3 || len(fraction) > 2 {
		return 0, false
	}
	for _, c := range whole + fraction {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, _ := strconv.ParseUint(whole, 10, 32)
	f, _ := strconv.ParseUint((fraction + "00")[:2], 10, 32)
	bps := n*100 + f
	if bps == 0 || bps > 10_000 {
		return 0, false
	}
	return uint32(bps), true
}

// span reads a length of time written as a number and a unit: 30s, 15m, 12h,
// 3d. A bare number is not accepted, because nobody agrees what one means.
func span(text string) (uint64, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "0" || text == "none" {
		return 0, nil
	}
	if len(text) < 2 {
		return 0, errors.New("no unit")
	}
	var unit uint64
	switch text[len(text)-1] {
	case 's':
		unit = 1
	case 'm':
		unit = 60
	case 'h':
		unit = 3_600
	case 'd':
		unit = 86_400
	default:
		return 0, errors.New("no unit")
	}
	n, err := strconv.ParseUint(text[:len(text)-1], 10, 32)
	if err != nil {
		return 0, err
	}
	return n * unit, nil
}

// putToVoters builds the call that puts an action to a treasury's members, and
// hands the proposer the link to sign it.
func (s *Service) putToVoters(
	ctx context.Context, r *request, treasury store.Treasury, action treasuryabi.Action, note string,
) error {
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	prepared, err := s.sender.ProposeOnChain(ctx, r.scope, treasury, r.member, action, note)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}

	waits := "It is only a proposal: nothing happens until the voters have passed it and its timelock has run."
	switch action.Kind {
	case treasuryabi.DenyRecipient, treasuryabi.Revoke:
		// The two that only ever stop something, and so do not wait.
		waits = "It is only a proposal until the voters pass it. This kind takes effect as soon as it " +
			"passes, with no timelock: it can only stop something."
	}
	return s.callLink(ctx, r, prepared,
		fmt.Sprintf("To put this to %s's voters: %s.", treasuryName(treasury), s.sender.DescribeAction(action)),
		"\n\n"+waits+" The page tells you its number once you've signed; the others then run /approve with it.")
}

// payFromContract proposes a payment out of a contract treasury.
func (s *Service) payFromContract(ctx context.Context, r *request, treasury store.Treasury) error {
	// The asset is this kind of treasury's to name and no part of the classic
	// command, so it is taken out of the line before the rest is read: left
	// in, the last words of a line are a memo, and "asset:XLM" would be one.
	extra, rest := named(r.msg, "asset")
	line := r.msg
	line.Args = strings.Join(rest, " ")
	args := payArguments(line)
	amountText, destination := args["amount"], args["to"]
	if amountText == "" || destination == "" {
		return r.reply(ctx, "Try: /pay 250 to GABC…")
	}
	amount, err := money.Parse(amountText)
	if err != nil {
		return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount.", amountText))
	}
	if amount <= 0 {
		return r.reply(ctx, "A payment has to be for more than nothing.")
	}
	if !payable(destination) {
		return r.reply(ctx, "That destination isn't somewhere a payment can go. It needs to be a G… or C… address.")
	}
	asset, err := s.sender.TreasuryAsset(extra["asset"])
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	action, err := s.sender.PayAction(asset, destination, amount)
	if err != nil {
		return err
	}
	return s.putToVoters(ctx, r, treasury, action, args["memo"])
}

// allow proposes adding a recipient, and disallow removing one.
func (s *Service) allow(ctx context.Context, r *request, adding bool) error {
	command := "allow"
	if !adding {
		command = "disallow"
	}
	args, rest := named(r.msg, "address", "treasury")
	address := args["address"]
	if address == "" && len(rest) > 0 {
		address = rest[0]
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], command)
	if err != nil || !ok {
		return err
	}
	if !payable(address) {
		return r.reply(ctx, fmt.Sprintf("Which address? Try: /%s GABC…", command))
	}
	kind := treasuryabi.AllowRecipient
	if !adding {
		kind = treasuryabi.DenyRecipient
	}
	return s.putToVoters(ctx, r, treasury, treasuryabi.Action{Kind: kind, Recipient: address}, "")
}

// voter proposes giving somebody a vote, changing its weight, or taking it
// away.
func (s *Service) voter(ctx context.Context, r *request) error {
	args, rest := named(r.msg, "member", "weight", "treasury")
	who := args["member"]
	if who == "" && len(rest) > 0 {
		who, rest = rest[0], rest[1:]
	}
	if _, ok := args["weight"]; !ok && len(rest) > 0 {
		args["weight"] = rest[0]
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], "voter")
	if err != nil || !ok {
		return err
	}
	if who == "" {
		return r.reply(ctx,
			"Whose vote? Try: /voter #3 — or /voter #3 weight:2, or weight:0 to take it away. "+
				"/members lists the numbers. An address works too: /voter GABC…")
	}

	// A member of this workspace by number, or an address outright. The
	// contract knows only addresses; the number is a convenience for finding
	// one somebody here has proved is theirs.
	address := who
	if !strkey.IsValidEd25519PublicKey(who) {
		n, convErr := strconv.ParseInt(strings.TrimPrefix(who, "#"), 10, 64)
		if convErr != nil || n <= 0 {
			return r.reply(ctx, fmt.Sprintf(
				"I couldn't read %q as a member number or an address. /members lists the numbers.", who))
		}
		member, findErr := s.store.Member(ctx, r.org.ID, store.MemberID(n))
		if errors.Is(findErr, store.ErrMemberNotFound) {
			return r.reply(ctx, fmt.Sprintf("There's no member #%d here. /members lists them.", n))
		}
		if findErr != nil {
			return findErr
		}
		if !member.HasAddress() {
			return r.reply(ctx, fmt.Sprintf(
				"Member #%d has no wallet here yet, so there is nothing for a vote to belong to. "+
					"They can /link one.", n))
		}
		address = member.Address
	}

	weight := uint64(1)
	if text, ok := args["weight"]; ok {
		weight, err = strconv.ParseUint(text, 10, 32)
		if err != nil {
			return r.reply(ctx, fmt.Sprintf("I couldn't read %q as a weight. It is a whole number; 0 takes the vote away.", text))
		}
	}
	return s.putToVoters(ctx, r, treasury, treasuryabi.Action{
		Kind: treasuryabi.SetMember, Member: &treasuryabi.Member{Address: address, Weight: uint32(weight)},
	}, "")
}

// rules shows a contract treasury's rules, or proposes changing them.
func (s *Service) rules(ctx context.Context, r *request) error {
	args, _ := named(r.msg, "quorum", "approval", "voting", "timelock", "limit", "window", "treasury")
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], "rules")
	if err != nil || !ok {
		return err
	}
	state, err := s.sender.ContractTreasuryState(ctx, r.scope, treasury)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}

	changing := false
	for _, key := range []string{"quorum", "approval", "voting", "timelock", "limit", "window"} {
		if _, ok := args[key]; ok {
			changing = true
		}
	}
	if !changing {
		return r.reply(ctx, fmt.Sprintf(
			"%s, held by a contract at\n%s\n\n%s\n\nLeft of the spending allowance: %s.\n\n"+
				"Read from the contract just now. To propose a change, name what should differ: "+
				"/rules timelock:2d — the rest stays as it is.",
			treasuryName(treasury), treasury.Address, describeState(state), api.TokenAmount(state.Spendable)))
	}
	if !r.role.AtLeast(chat.RoleProposer) {
		return r.reply(ctx, "Proposing a change to the rules needs the proposer role in this workspace.")
	}

	policy := state.Policy
	if reply := applyRules(&policy, args); reply != "" {
		return r.reply(ctx, reply)
	}
	return s.putToVoters(ctx, r, treasury, treasuryabi.Action{Kind: treasuryabi.SetPolicy, Policy: &policy}, "")
}

// stop proposes stopping a proposal that has passed and not yet been carried
// out.
func (s *Service) stop(ctx context.Context, r *request) error {
	treasury, id, ok, err := s.contractProposalNumber(ctx, r, "stop")
	if err != nil || !ok {
		return err
	}
	return s.putToVoters(ctx, r, treasury, treasuryabi.Action{Kind: treasuryabi.Revoke, Target: id}, "")
}

// contractProposalNumber reads which proposal, of which contract treasury, a
// contract-only command names.
func (s *Service) contractProposalNumber(
	ctx context.Context, r *request, command string,
) (store.Treasury, uint32, bool, error) {
	args, rest := named(r.msg, "id", "treasury")
	text := args["id"]
	if text == "" && len(rest) > 0 {
		text = rest[0]
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], command)
	if err != nil || !ok {
		return store.Treasury{}, 0, false, err
	}
	id, ok := proposalNumber(text)
	if !ok {
		return store.Treasury{}, 0, false, r.reply(ctx, fmt.Sprintf(
			"Which proposal? Try: /%s 3 — /proposals lists them.", command))
	}
	return treasury, id, true, nil
}

func proposalNumber(text string) (uint32, bool) {
	n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(text), "#"), 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

// chainProposal loads the proposal a command names from a contract treasury.
// A nil proposal with a nil error means a reply was already sent.
func (s *Service) chainProposal(
	ctx context.Context, r *request, treasury store.Treasury, text string,
) (*api.ChainProposal, error) {
	id, ok := proposalNumber(text)
	if !ok {
		return nil, r.reply(ctx, fmt.Sprintf(
			"Which proposal? %s numbers its own: /proposals lists them.", treasuryName(treasury)))
	}
	p, err := s.sender.ChainProposalByID(ctx, r.scope, treasury, id)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return nil, failed
	}
	return p, nil
}

// vote hands a member the link to cast their vote on a proposal.
func (s *Service) vote(ctx context.Context, r *request, treasury store.Treasury, text string, approve bool) error {
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	p, err := s.chainProposal(ctx, r, treasury, text)
	if err != nil || p == nil {
		return err
	}
	if p.Status != treasuryabi.Open || p.Settles != "" {
		return r.reply(ctx, fmt.Sprintf("Proposal %d is not taking votes any more. %s",
			p.ID, standing(p, time.Now())))
	}

	prepared, err := s.sender.VoteOnChain(ctx, r.scope, treasury, r.member, p, approve)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	side := "against"
	if approve {
		side = "for"
	}
	lead := fmt.Sprintf("Your vote %s proposal %d of %s, which is to %s.",
		side, p.ID, treasuryName(treasury), s.sender.DescribeAction(p.Action))
	if p.Note != "" {
		lead += "\nIts proposer's note: " + p.Note
	}
	return s.callLink(ctx, r, prepared, lead,
		fmt.Sprintf("\n\nSo far: weight %d for, %d against. Voting closes %s, or as soon as the "+
			"outcome can no longer change.", p.Yes, p.No, when(p.ClosesAt)))
}

// standing says where a proposal is in its life.
func standing(p *api.ChainProposal, now time.Time) string {
	switch p.Status {
	case treasuryabi.Executed:
		return "It has been carried out."
	case treasuryabi.Rejected:
		return fmt.Sprintf("It was rejected, with weight %d for and %d against.", p.Yes, p.No)
	case treasuryabi.Cancelled:
		return "It was withdrawn or stopped."
	case treasuryabi.Passed:
		if uint64(now.Unix()) < p.ExecutableAt {
			return fmt.Sprintf("It passed, and can be carried out from %s.", when(p.ExecutableAt))
		}
		return fmt.Sprintf("It passed and its timelock has run: /execute %d carries it out.", p.ID)
	}
	switch p.Settles {
	case treasuryabi.Passed:
		return fmt.Sprintf("Its outcome is settled — it passes, with weight %d for and %d against — "+
			"and /execute %d records that and carries it out when its timelock allows.", p.Yes, p.No, p.ID)
	case treasuryabi.Rejected:
		return fmt.Sprintf("Its outcome is settled — it is rejected, with weight %d for and %d against.", p.Yes, p.No)
	}
	return fmt.Sprintf("Voting is open until %s: weight %d for, %d against.", when(p.ClosesAt), p.Yes, p.No)
}

func when(unix uint64) string {
	return time.Unix(int64(unix), 0).UTC().Format("2 Jan 15:04 MST")
}

// executeOnChain carries out a contract treasury's proposal, or says why not
// yet.
//
// Open to anyone the bot talks to, as it is for a classic treasury and for the
// same reason: the votes are the authorisation, and the contract itself takes
// this call from anybody.
func (s *Service) executeOnChain(ctx context.Context, r *request, treasury store.Treasury, text string) error {
	p, err := s.chainProposal(ctx, r, treasury, text)
	if err != nil || p == nil {
		return err
	}

	switch {
	case p.Status == treasuryabi.Open && p.Settles == "":
		return r.reply(ctx, fmt.Sprintf("Proposal %d can't be carried out yet. %s", p.ID, standing(p, time.Now())))

	case p.Status == treasuryabi.Open && p.Settles == treasuryabi.Rejected:
		// Nobody has an incentive to close a proposal that lost, and one left
		// open would misreport itself to everyone reading. Recorded here.
		if _, err := s.sender.SettleOnChain(ctx, r.scope, treasury, p.ID); err != nil {
			if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
				return failed
			}
		}
		return r.reply(ctx, fmt.Sprintf(
			"Proposal %d is rejected, with weight %d for and %d against. That is recorded on the contract now.",
			p.ID, p.Yes, p.No))

	case p.Status == treasuryabi.Open:
		// Settled in its favour and not yet recorded. Recording it is what
		// starts the clock a member can read, so do that, then look again.
		if _, err := s.sender.SettleOnChain(ctx, r.scope, treasury, p.ID); err != nil {
			if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
				return failed
			}
		}
		p, err = s.chainProposal(ctx, r, treasury, text)
		if err != nil || p == nil {
			return err
		}
	}

	if p.Status != treasuryabi.Passed {
		return r.reply(ctx, fmt.Sprintf("Proposal %d can't be carried out. %s", p.ID, standing(p, time.Now())))
	}
	if uint64(time.Now().Unix()) < p.ExecutableAt {
		return r.reply(ctx, fmt.Sprintf(
			"Proposal %d has passed, and its timelock runs until %s. That wait is the voters' chance "+
				"to stop it: /stop %d proposes that. Run /execute %d again once it is over.",
			p.ID, when(p.ExecutableAt), p.ID, p.ID))
	}

	result, err := s.sender.ExecuteOnChain(ctx, r.scope, treasury, p.ID)
	var refusal *api.ContractRefusal
	if errors.As(err, &refusal) && refusal.Code == treasuryabi.NotExecutable {
		// My clock and the ledger's differ by a few seconds, and the ledger's
		// is the one that counts.
		return r.reply(ctx, fmt.Sprintf(
			"Proposal %d is a few seconds short of its timelock by the network's clock. Try /execute %d again in a moment.",
			p.ID, p.ID))
	}
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	return r.reply(ctx, fmt.Sprintf(
		"Proposal %d is carried out: %s did %s.\n\nTransaction %s, ledger %d.",
		p.ID, treasuryName(treasury), s.sender.DescribeAction(p.Action), result.Hash, result.Ledger))
}

// withdraw hands a proposer the link to withdraw their own open proposal.
func (s *Service) withdraw(ctx context.Context, r *request, treasury store.Treasury, text string) error {
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	p, err := s.chainProposal(ctx, r, treasury, text)
	if err != nil || p == nil {
		return err
	}
	if p.Status != treasuryabi.Open {
		return r.reply(ctx, fmt.Sprintf("Proposal %d is not open, so there is nothing to withdraw. %s",
			p.ID, standing(p, time.Now())))
	}
	if p.Proposer != r.member.Address {
		return r.reply(ctx, fmt.Sprintf(
			"Proposal %d was made from %s, and only that wallet can withdraw it — there is no admin "+
				"over a treasury held by a contract. Vote against it with /reject %d.",
			p.ID, p.Proposer, p.ID))
	}
	prepared, err := s.sender.WithdrawOnChain(ctx, r.scope, treasury, r.member, p)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	return s.callLink(ctx, r, prepared,
		fmt.Sprintf("To withdraw proposal %d of %s, which is to %s.",
			p.ID, treasuryName(treasury), s.sender.DescribeAction(p.Action)), "")
}

// chainProposals writes one contract treasury's recent proposals.
func (s *Service) chainProposals(ctx context.Context, r *request, treasury store.Treasury, b *strings.Builder) error {
	list, err := s.sender.ChainProposals(ctx, r.scope, treasury, 8)
	if err != nil {
		return err
	}
	fmt.Fprintf(b, "%s (held by a contract):\n", treasuryName(treasury))
	if len(list) == 0 {
		b.WriteString("\nNo proposals yet. /pay starts one.\n")
		return nil
	}
	now := time.Now()
	for i := range list {
		p := &list[i]
		fmt.Fprintf(b, "\n%d · %s\n  %s", p.ID, s.sender.DescribeAction(p.Action), standing(p, now))
		if p.Note != "" {
			fmt.Fprintf(b, "\n  Note: %s", p.Note)
		}
	}
	b.WriteString("\n\n/approve <number> votes for one and /reject <number> against; /execute <number> " +
		"carries out one that has passed.\n")
	return nil
}

// reject hands a member the link to vote against a proposal.
func (s *Service) reject(ctx context.Context, r *request) error {
	args, rest := named(r.msg, "id", "treasury")
	text := args["id"]
	if text == "" && len(rest) > 0 {
		text = rest[0]
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], "reject")
	if err != nil || !ok {
		return err
	}
	return s.vote(ctx, r, treasury, text, false)
}

// spend pays out of a contract treasury under its rolling allowance, on one
// member's signature.
func (s *Service) spend(ctx context.Context, r *request) error {
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	args, rest := named(r.msg, "amount", "to", "asset", "treasury")
	for _, word := range rest {
		switch {
		case strings.EqualFold(word, "to"):
		case args["amount"] == "":
			args["amount"] = word
		case args["to"] == "":
			args["to"] = word
		}
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], "spend")
	if err != nil || !ok {
		return err
	}
	if args["amount"] == "" || args["to"] == "" {
		return r.reply(ctx,
			"Try: /spend 20 to GABC…\n\nThat pays out of the treasury on your signature alone, which "+
				"it allows only up to the limit its voters set and only to somebody on its list. /rules shows both.")
	}
	amount, err := money.Parse(args["amount"])
	if err != nil || amount <= 0 {
		return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount.", args["amount"]))
	}
	if !payable(args["to"]) {
		return r.reply(ctx, "That destination isn't somewhere a payment can go. It needs to be a G… or C… address.")
	}
	asset, err := s.sender.TreasuryAsset(args["asset"])
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	prepared, err := s.sender.SpendOnChain(ctx, r.scope, treasury, r.member, asset, args["to"], amount)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	return s.callLink(ctx, r, prepared,
		fmt.Sprintf("To spend %s from %s, to %s, on your signature alone.",
			amount.Display(), treasuryName(treasury), args["to"]),
		"\n\nThis one is not a proposal. It is paid as soon as you sign.")
}

// deposit moves money from a member's own wallet into a contract treasury.
func (s *Service) deposit(ctx context.Context, r *request) error {
	args, rest := named(r.msg, "amount", "asset", "treasury")
	for _, word := range rest {
		switch {
		case args["amount"] == "":
			args["amount"] = word
		case args["asset"] == "":
			args["asset"] = word
		}
	}

	list, err := s.store.Treasuries(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if len(list) == 1 && list[0].Kind == store.TreasuryClassic && args["treasury"] == "" {
		// Nothing to build: a classic account is paid like any other.
		return r.reply(ctx, fmt.Sprintf(
			"%s is an ordinary account, so it is paid like one: send to\n%s\nfrom any wallet. "+
				"/balance shows it arrive.", treasuryName(list[0]), list[0].Address))
	}
	treasury, ok, err := s.onlyContract(ctx, r, args["treasury"], "deposit")
	if err != nil || !ok {
		return err
	}
	if !r.member.HasAddress() {
		return s.needsWallet(ctx, r)
	}
	if args["amount"] == "" {
		return r.reply(ctx, "How much? Try: /deposit 100 — or /deposit 100 XLM.")
	}
	amount, err := money.Parse(args["amount"])
	if err != nil || amount <= 0 {
		return r.reply(ctx, fmt.Sprintf("I couldn't read %q as an amount.", args["amount"]))
	}
	asset, err := s.sender.TreasuryAsset(args["asset"])
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	prepared, err := s.sender.DepositOnChain(ctx, r.scope, treasury, r.member, asset, amount)
	if handled, failed := s.refusedOnChain(ctx, r, treasury, err); handled || failed != nil {
		return failed
	}
	return s.callLink(ctx, r, prepared,
		fmt.Sprintf("To move %s from your wallet %s into %s.",
			amount.Display(), r.member.Address, treasuryName(treasury)),
		"\n\nOnce it is in, it leaves only by the voters' decision. Anybody can also pay the "+
			"contract's address directly, from any wallet that can send to a contract.")
}

// daoTreasuryCommands are registered alongside the rest.
func (s *Service) daoTreasuryCommands() []*command {
	which := chat.Option{Name: "treasury", Description: "Which treasury, if there are several", Type: chat.OptString}
	number := chat.Option{Name: "id", Description: "The proposal number", Type: chat.OptString, Required: true}
	return []*command{
		{
			name:        "create-treasury",
			description: "Create a treasury its members vote in",
			options: []chat.Option{
				{Name: "name", Description: "What to call it", Type: chat.OptString, Required: true},
				{Name: "voting", Description: "How long voting stays open, e.g. 3d", Type: chat.OptString},
				{Name: "timelock", Description: "How long a passed proposal waits, e.g. 1d", Type: chat.OptString},
				{Name: "quorum", Description: "Per cent of voting weight that must vote", Type: chat.OptString},
				{Name: "approval", Description: "Per cent of those voting that must agree", Type: chat.OptString},
				{Name: "recipients", Description: "Addresses it may pay from the start, comma separated", Type: chat.OptString},
			},
			// Admin, as linking a treasury is. It confers nothing over the
			// money: the contract that results has no admin at all.
			minRole: chat.RoleAdmin,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.createTreasury(ctx, r) },
		},
		{
			name:        "deposit",
			description: "Move money from your wallet into the treasury",
			options: []chat.Option{
				{Name: "amount", Description: "How much", Type: chat.OptString, Required: true},
				{Name: "asset", Description: "XLM, or an asset code", Type: chat.OptString},
				which,
			},
			minRole: chat.RoleObserver,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.deposit(ctx, r) },
		},
		{
			name:        "reject",
			description: "Vote against a proposal",
			options:     []chat.Option{number, which},
			// Observer, like /approve: it hands somebody a link, and whether
			// their vote counts is the contract's to say.
			minRole: chat.RoleObserver,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.reject(ctx, r) },
		},
		{
			name:        "stop",
			description: "Propose stopping a proposal that has passed",
			options:     []chat.Option{number, which},
			minRole:     chat.RoleProposer,
			needs:       needsSoroban,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.stop(ctx, r) },
		},
		{
			name:        "allow",
			description: "Propose an address the treasury may pay",
			options: []chat.Option{
				{Name: "address", Description: "The G… or C… address", Type: chat.OptString, Required: true},
				which,
			},
			minRole: chat.RoleProposer,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.allow(ctx, r, true) },
		},
		{
			name:        "disallow",
			description: "Propose that the treasury stop paying an address",
			options: []chat.Option{
				{Name: "address", Description: "The G… or C… address", Type: chat.OptString, Required: true},
				which,
			},
			minRole: chat.RoleProposer,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.allow(ctx, r, false) },
		},
		{
			name:        "voter",
			description: "Propose giving a member a vote, or changing it",
			options: []chat.Option{
				{Name: "member", Description: "A member number from /members, or an address", Type: chat.OptString, Required: true},
				{Name: "weight", Description: "The weight of their vote; 0 takes it away", Type: chat.OptString},
				which,
			},
			minRole: chat.RoleProposer,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.voter(ctx, r) },
		},
		{
			name:        "rules",
			description: "Show the treasury's rules, or propose changing them",
			options: []chat.Option{
				{Name: "quorum", Description: "Per cent of voting weight that must vote", Type: chat.OptString},
				{Name: "approval", Description: "Per cent of those voting that must agree", Type: chat.OptString},
				{Name: "voting", Description: "How long voting stays open, e.g. 3d", Type: chat.OptString},
				{Name: "timelock", Description: "How long a passed proposal waits, e.g. 1d", Type: chat.OptString},
				{Name: "limit", Description: "What one member may spend without a proposal", Type: chat.OptString},
				{Name: "window", Description: "Over how long that limit applies, e.g. 1d", Type: chat.OptString},
				which,
			},
			// Reading them is for anyone the bot talks to; proposing a change
			// is checked inside, where it is known which was asked for.
			minRole: chat.RoleObserver,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.rules(ctx, r) },
		},
		{
			name:        "spend",
			description: "Pay from the treasury under its allowance, without a proposal",
			options: []chat.Option{
				{Name: "amount", Description: "How much", Type: chat.OptString, Required: true},
				{Name: "to", Description: "The address to pay", Type: chat.OptString, Required: true},
				{Name: "asset", Description: "XLM, or an asset code", Type: chat.OptString},
				which,
			},
			minRole: chat.RoleProposer,
			needs:   needsSoroban,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.spend(ctx, r) },
		},
	}
}
