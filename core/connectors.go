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
	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// setupLinkLifetime bounds the link to a connector's set-up page. Long enough
// to find a token in another tab; it carries the authority to point a
// connector somewhere, so not longer.
const setupLinkLifetime = 15 * time.Minute

// grantLinkLifetime bounds the link to a grant's signing page. Signatures are
// collected from several people, and the envelope itself lasts an hour.
const grantLinkLifetime = 45 * time.Minute

// rowsShown bounds how much of a read or a draft goes into one reply.
const rowsShown = 12

// connectorCmd registers, grants, revokes and removes connectors.
//
// Four things an admin can ask for and only three they can do alone.
// Registering, revoking and removing each only describe or take away. Granting
// adds something stelfin will do on the workspace's behalf, so all an admin can
// do is ask: the reply is a link to a page where the treasury's signers sign.
func (s *Service) connectorCmd(ctx context.Context, r *request) error {
	words := strings.Fields(r.msg.Args)
	word := func(i int) string {
		if i < len(words) {
			return words[i]
		}
		return ""
	}
	action := strings.ToLower(firstOf(r.msg.Options["action"], word(0)))
	name := strings.ToLower(firstOf(r.msg.Options["name"], word(1)))
	third := firstOf(r.msg.Options["kind"], r.msg.Options["per-hour"], word(2))

	if action == "" || name == "" {
		return r.reply(ctx, connectorUsage)
	}

	switch action {
	case "add":
		return s.connectorAdd(ctx, r, name, strings.ToLower(third))
	case "setup":
		return s.connectorSetup(ctx, r, name)
	case "grant":
		return s.connectorGrant(ctx, r, name, third)
	case "revoke":
		err := s.store.RevokeGrant(ctx, r.org.ID, name, r.member.ID)
		switch {
		case errors.Is(err, store.ErrNoGrant):
			return r.reply(ctx, fmt.Sprintf("%q has no grant in force, so there is nothing to revoke.", name))
		case err != nil:
			return err
		}
		s.log.Info("connector grant revoked", "org", r.org.ID, "connector", name, "by", r.member.ID)
		return r.reply(ctx, fmt.Sprintf(
			"Revoked. I won't call %q again until the treasury's signers grant it again.\n\n"+
				"That took no signatures on purpose: stopping a connector should never need a quorum.", name))
	case "remove":
		err := s.store.RemoveConnector(ctx, r.org.ID, name, r.member.ID)
		switch {
		case errors.Is(err, store.ErrNoConnector):
			return r.reply(ctx, fmt.Sprintf("There's no connector called %q here. /connectors lists them.", name))
		case err != nil:
			return err
		}
		s.log.Info("connector removed", "org", r.org.ID, "connector", name, "by", r.member.ID)
		return r.reply(ctx, fmt.Sprintf("Removed %q, and its grant with it.", name))
	default:
		return r.reply(ctx, connectorUsage)
	}
}

const connectorUsage = "Try one of:\n\n" +
	"/connector add payroll sheets — also: mcp, webhook\n" +
	"/connector setup rates — where it is, and its token (on a page, not here)\n" +
	"/connector grant payroll — asks the treasury's signers\n" +
	"/connector revoke payroll\n" +
	"/connector remove payroll\n\n" +
	"/connectors lists what's here."

func firstOf(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func (s *Service) connectorAdd(ctx context.Context, r *request, name, kind string) error {
	if kind == "" {
		return r.reply(ctx, "What kind? Try: /connector add "+name+" sheets — or mcp, or webhook.")
	}
	row, err := s.sender.RegisterConnector(ctx, r.scope, r.member.ID, name, kind)
	switch {
	case errors.Is(err, api.ErrConnectorName):
		return r.reply(ctx, "A connector's name is lower-case letters, digits and hyphens, starting with a letter — like payroll or fx-rates.")
	case errors.Is(err, api.ErrConnectorKind):
		return r.reply(ctx, fmt.Sprintf("I don't know a kind of connector called %q. They are: sheets, mcp, webhook.", kind))
	case errors.Is(err, api.ErrConnectorsUnavailable):
		return r.reply(ctx, "That kind of connector isn't available on this deployment. Whoever runs it has to configure it first.")
	case errors.Is(err, store.ErrConnectorExists):
		return r.reply(ctx, fmt.Sprintf("There's already a connector called %q here. /connector remove %s first to replace it.", name, name))
	case err != nil:
		return err
	}
	s.log.Info("connector registered", "org", r.org.ID, "connector", row.Name, "kind", row.Kind, "by", r.member.ID)

	if row.Ready {
		share := ""
		if account := s.sender.SheetsAccount(); account != "" {
			share = fmt.Sprintf("Share the spreadsheet with %s as a viewer — that's all it can ever be.\n\n", account)
		}
		return r.reply(ctx, fmt.Sprintf(
			"Registered %q. That grants it nothing yet.\n\n%s"+
				"Next: /connector grant %s, which asks the treasury's signers to authorise it.",
			row.Name, share, row.Name))
	}
	return s.connectorSetupLink(ctx, r, row, fmt.Sprintf("Registered %q. That grants it nothing yet.", row.Name))
}

func (s *Service) connectorSetup(ctx context.Context, r *request, name string) error {
	row, err := s.store.ConnectorByName(ctx, r.org.ID, name)
	switch {
	case errors.Is(err, store.ErrNoConnector):
		return r.reply(ctx, fmt.Sprintf("There's no connector called %q here. /connectors lists them.", name))
	case err != nil:
		return err
	}
	if row.Kind == store.ConnectorSheets {
		return r.reply(ctx, "A spreadsheet connector has nothing to set up. /connector grant "+name+" is the next step.")
	}
	lead := fmt.Sprintf("Setting %q up again.", row.Name)
	if row.Ready {
		lead += " If it has a grant, that grant ends when you save: the signers agreed to the connector as it was."
	}
	return s.connectorSetupLink(ctx, r, row, lead)
}

func (s *Service) connectorSetupLink(ctx context.Context, r *request, row store.Connector, lead string) error {
	link, err := r.links.IssueLinkLink(r.scope, api.SetupToken(row.ID), time.Now().Add(setupLinkLifetime))
	if err != nil {
		return err
	}
	return r.reply(ctx, fmt.Sprintf(
		"%s\n\nTell me where it is, and its access token if it has one, on this page — "+
			"not here, where a room can read it:\n%s\n\n"+
			"The link expires in %d minutes. After that: /connector grant %s.",
		lead, link, int(setupLinkLifetime.Minutes()), row.Name))
}

func (s *Service) connectorGrant(ctx context.Context, r *request, name, rate string) error {
	if s.challenges == nil {
		return r.reply(ctx, "Granting a connector takes signatures, and signing isn't available on this deployment yet.")
	}
	perHour := 0
	if rate != "" {
		n, err := strconv.Atoi(rate)
		if err != nil || n < 1 || n > 3600 {
			return r.reply(ctx, fmt.Sprintf("I couldn't read %q as calls an hour. Between 1 and 3600, like: /connector grant %s 30", rate, name))
		}
		perHour = n
	}
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}
	identity, err := s.store.IdentityFor(ctx, r.org.ID, r.actor.Channel, r.actor.UserID)
	if err != nil {
		return err
	}

	request, err := s.sender.PrepareGrant(ctx, r.scope, identity, r.member.ID, name, treasury.ID, perHour)
	switch {
	case errors.Is(err, store.ErrNoConnector):
		return r.reply(ctx, fmt.Sprintf("There's no connector called %q here. /connectors lists them.", name))
	case errors.Is(err, api.ErrConnectorNotReady):
		return r.reply(ctx, fmt.Sprintf("%q hasn't been set up yet, so there's nothing definite to grant. /connector setup %s first.", name, name))
	case errors.Is(err, api.ErrConnectorsUnavailable), errors.Is(err, api.ErrLinkingUnavailable):
		return r.reply(ctx, "Connectors aren't available on this deployment.")
	case err != nil:
		return err
	}

	link, err := r.links.IssueLinkLink(r.scope, request.Challenge.Hash, time.Now().Add(grantLinkLifetime))
	if err != nil {
		return err
	}
	return r.reply(ctx, fmt.Sprintf(
		"This asks the signers of %s to let me use %q:\n\n%s\n"+
			"It takes the same signing weight a payment takes. Open this and collect "+
			"signatures — the page shows the statement and checks that the envelope "+
			"commits to exactly it:\n%s\n\n"+
			"The link expires in %d minutes. Pass the envelope from the page to the other "+
			"signers, not this link. Nothing is granted until they have signed.",
		treasuryName(treasury), request.Connector.Name, humanStatement(request.Statement), link,
		int(grantLinkLifetime.Minutes())))
}

// humanStatement renders a grant's statement for a chat reply: the lines a
// person decides on, without the format marker and the digests.
func humanStatement(statement string) string {
	var b strings.Builder
	for _, line := range strings.Split(statement, "\n") {
		key, value, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch key {
		case "endpoint_digest", "surface", "workspace", "network", "treasury":
			continue
		}
		fmt.Fprintf(&b, "· %s: %s\n", key, value)
	}
	return b.String()
}

// connectors lists what is registered and where each one stands.
func (s *Service) connectors(ctx context.Context, r *request) error {
	list, err := s.store.Connectors(ctx, r.org.ID)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "No connectors here yet. An admin can add one: /connector add payroll sheets")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Connectors for %s:\n", r.org.DisplayName)
	for _, c := range list {
		where := c.EndpointHost
		if c.Kind == store.ConnectorSheets {
			where = "Google Sheets"
		}
		state := "needs set-up"
		if c.Ready {
			state = "not granted"
			grant, err := s.store.GrantInForce(ctx, r.org.ID, c.Name)
			switch {
			case err == nil:
				state = fmt.Sprintf("granted, %d calls an hour, signed by %d", grant.PerHour, len(grant.SignedBy))
			case !errors.Is(err, store.ErrNoGrant):
				return err
			}
		}
		if where == "" {
			where = "nowhere yet"
		}
		fmt.Fprintf(&b, "\n· %s — %s, %s — %s", c.Name, c.Kind, where, state)
	}
	return r.reply(ctx, b.String())
}

// connectorRefusal turns the errors a read can end in into something a member
// can act on. handled is false for anything that is not one of them.
func connectorRefusal(name string, err error) (reply string, handled bool) {
	switch {
	case errors.Is(err, store.ErrNoConnector):
		return fmt.Sprintf("There's no connector called %q here. /connectors lists them.", name), true
	case errors.Is(err, api.ErrConnectorNotReady):
		return fmt.Sprintf("%q hasn't been set up yet. An admin can: /connector setup %s", name, name), true
	case errors.Is(err, connector.ErrSurfaceChanged):
		// Said plainly, because it is the case the whole pinning exists for and
		// the right response is suspicion rather than a retry.
		return fmt.Sprintf(
			"I didn't call %q: what it offers has changed since the treasury's signers granted it. "+
				"That may be an ordinary update or it may not. An admin can look, and then "+
				"/connector setup %s and /connector grant %s to put the new version to the signers.",
			name, name, name), true
	case errors.Is(err, connector.ErrNotGranted):
		return fmt.Sprintf(
			"%q hasn't been granted. Using it takes the treasury's signers: an admin can ask them with /connector grant %s.",
			name, name), true
	case errors.Is(err, connector.ErrCapabilityNotGranted):
		return fmt.Sprintf("%q's grant doesn't cover that.", name), true
	case errors.Is(err, connector.ErrRateExceeded):
		return fmt.Sprintf("%q has been called as often as its grant allows this hour. Try again later.", name), true
	case errors.Is(err, api.ErrConnectorsUnavailable):
		return "Connectors aren't available on this deployment.", true
	case errors.Is(err, api.ErrConnectorFailed):
		return fmt.Sprintf(
			"I couldn't read %q. It didn't answer, or answered with something I can't use. "+
				"Nothing was changed.", name), true
	}
	return "", false
}

// read shows what a connector returns.
func (s *Service) read(ctx context.Context, r *request) error {
	name, selector := nameAndRest(r.msg, "connector", "what")
	if name == "" {
		return r.reply(ctx, "Read what? Try: /read payroll <spreadsheet id>!A1:C20")
	}
	seen, _, err := s.sender.ReadConnector(ctx, r.scope, name, selector)
	if reply, handled := connectorRefusal(name, err); handled {
		return r.reply(ctx, reply)
	}
	if err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s returned %d row(s).", name, len(seen.Rows))
	if len(seen.Columns) > 0 {
		fmt.Fprintf(&b, "\n\n%s", cells(seen.Columns))
	} else {
		b.WriteString("\n")
	}
	for i, row := range seen.Rows {
		if i == rowsShown {
			fmt.Fprintf(&b, "\n…and %d more.", len(seen.Rows)-rowsShown)
			break
		}
		fmt.Fprintf(&b, "\n%s", cells(row))
	}
	if seen.Truncated {
		b.WriteString("\n\nThere was more than I read.")
	}
	// The one thing to say about text from outside, every time it is shown.
	b.WriteString("\n\nThis is what it said, not something I checked.")
	return r.reply(ctx, b.String())
}

// cells renders one row. What a connector returned is unwrapped here, in the
// open, to be shown to a person as text and for nothing else.
func cells(row []connector.Untrusted[string]) string {
	out := make([]string, 0, len(row))
	for _, c := range row {
		text := strings.Join(strings.Fields(c.Unwrap()), " ")
		if len([]rune(text)) > 60 {
			text = string([]rune(text)[:60]) + "…"
		}
		out = append(out, text)
	}
	return strings.Join(out, " | ")
}

// nameAndRest reads a connector's name and whatever follows it.
func nameAndRest(m chat.Inbound, nameOption, restOption string) (name, rest string) {
	name, rest = strings.TrimSpace(m.Options[nameOption]), strings.TrimSpace(m.Options[restOption])
	if name != "" {
		return strings.ToLower(name), rest
	}
	first, after, _ := strings.Cut(strings.TrimSpace(m.Args), " ")
	return strings.ToLower(first), strings.TrimSpace(after)
}

// payroll reads a connector and keeps what it found as a draft.
//
// A draft, and not a proposal. The person who keeps the sheet and the person
// who answers for proposing it are often different people, and both of them
// should see what was read before anyone is asked to sign it.
func (s *Service) payroll(ctx context.Context, r *request) error {
	name, rest := nameAndRest(r.msg, "connector", "what")
	memo := strings.TrimSpace(r.msg.Options["memo"])
	if at := strings.Index(rest, " memo:"); at >= 0 && memo == "" {
		rest, memo = strings.TrimSpace(rest[:at]), strings.TrimSpace(rest[at+len(" memo:"):])
	}
	if name == "" {
		return r.reply(ctx, "Which connector? Try: /payroll payroll <spreadsheet id>!A2:C40 memo:March salaries")
	}

	draft, err := s.sender.DraftPayroll(ctx, r.scope, r.member.ID, name, rest, memo)
	if reply, handled := connectorRefusal(name, err); handled {
		return r.reply(ctx, reply)
	}
	var unreadable *api.BatchError
	switch {
	case errors.As(err, &unreadable):
		var b strings.Builder
		fmt.Fprintf(&b, "I read %s and can't make a payroll of it: %d row(s) can't be paid as written.\n", name, len(unreadable.Rows))
		for i, row := range unreadable.Rows {
			if i == rowsShown {
				fmt.Fprintf(&b, "\n…and %d more.", len(unreadable.Rows)-rowsShown)
				break
			}
			fmt.Fprintf(&b, "\n· row %d: %s", row.Line, row.Reason)
		}
		b.WriteString("\n\nNothing was drafted. Fix the sheet and run /payroll again — I won't pay the rows I could read and skip the ones I couldn't.")
		return r.reply(ctx, b.String())
	case errors.Is(err, api.ErrNotTabular), errors.Is(err, api.ErrBatchEmpty),
		errors.Is(err, api.ErrBatchTooLarge), errors.Is(err, api.ErrMemoTooLong):
		return r.reply(ctx, "I can't make a payroll of that: "+afterPrefix(err)+".")
	case err != nil:
		return err
	}

	return r.reply(ctx, describeDraft(draft)+fmt.Sprintf(
		"\n\nNothing has been proposed. /propose %d puts it to the treasury's signers; /discard %d drops it.",
		draft.ID, draft.ID))
}

// afterPrefix drops the package prefix from one of our own errors, for a reply.
func afterPrefix(err error) string {
	text := err.Error()
	if _, rest, ok := strings.Cut(text, ": "); ok {
		return rest
	}
	return text
}

func describeDraft(d store.Draft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Draft #%d: %d payment(s), %s in total.", d.ID, len(d.Rows), d.Total.Display())
	if d.AgentToken != 0 {
		fmt.Fprintf(&b, " Written by an agent (token %q), not read from a sheet.", d.AgentLabel)
	}
	if d.Memo != "" {
		fmt.Fprintf(&b, " Memo: %q.", d.Memo)
	}
	b.WriteString("\n")
	for i, row := range d.Rows {
		if i == rowsShown {
			fmt.Fprintf(&b, "\n…and %d more.", len(d.Rows)-rowsShown)
			break
		}
		amount, _ := money.Parse(row.Amount)
		fmt.Fprintf(&b, "\n· row %d: %s to %s", row.Line, amount.Display(), row.Destination)
		if row.Label != "" && row.Label != row.Destination {
			fmt.Fprintf(&b, " (%s)", row.Label)
		}
	}
	return b.String()
}

// drafts lists recent drafts.
func (s *Service) drafts(ctx context.Context, r *request) error {
	list, err := s.store.Drafts(ctx, r.org.ID, 10)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return r.reply(ctx, "No drafts here. /payroll <connector> <range> reads one from a connector.")
	}
	var b strings.Builder
	b.WriteString("Recent drafts:\n")
	for _, d := range list {
		fmt.Fprintf(&b, "\n#%d · %d payment(s) · %s · %s", d.ID, len(d.Rows), d.Total.Display(), d.Status)
		if d.AgentToken != 0 {
			fmt.Fprintf(&b, " · by agent %q", d.AgentLabel)
		}
		if d.Proposal != 0 {
			fmt.Fprintf(&b, " as proposal #%d", d.Proposal)
		}
	}
	return r.reply(ctx, b.String())
}

func draftNumber(m chat.Inbound) (store.DraftID, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(argument(m, "draft"), "#"), 10, 64)
	return store.DraftID(n), err == nil && n > 0
}

// propose puts a draft to the treasury's signers, as it was reviewed.
func (s *Service) propose(ctx context.Context, r *request) error {
	id, ok := draftNumber(r.msg)
	if !ok {
		return r.reply(ctx, "Which draft? Try: /propose 3 — /drafts lists them.")
	}
	treasury, err := s.soleTreasury(ctx, r)
	if err != nil || treasury.ID == 0 {
		return err
	}

	view, draft, err := s.sender.ProposeDraft(ctx, r.scope, id, treasury.ID, r.member.ID)
	var over *api.CeilingError
	var notReady *api.DestinationError
	switch {
	case errors.Is(err, store.ErrNoDraft):
		return r.reply(ctx, fmt.Sprintf("There's no open draft #%d here. /drafts lists them.", id))
	case errors.As(err, &over):
		return r.reply(ctx, overCeilingReply(over))
	case errors.As(err, &notReady):
		return r.reply(ctx, fmt.Sprintf(
			"I haven't put draft #%d to the signers, because one of its payments wouldn't land: %s %s.\n\n"+
				"The draft is still open. Once that destination is ready, /propose %d again.",
			id, notReady.Address, notReady.Reason, id))
	case errors.Is(err, store.ErrProposalAlreadyOpen):
		return r.reply(ctx, fmt.Sprintf(
			"%s already has a proposal open, and one account can only have one at a time. "+
				"Finish that one, then /propose %d again — the draft is still open.",
			treasuryName(treasury), id))
	case err != nil && view == nil:
		return err
	}
	if err != nil {
		// The proposal was opened and the draft could not be marked. Say what
		// is true; the log has the rest.
		s.log.Error("draft proposed but not closed", "org", r.org.ID, "draft", id, "error", err)
	}

	link, err := r.links.IssueApproveLink(r.scope, view.Proposal.ID, time.Now().Add(approveLinkLifetime))
	if err != nil {
		return err
	}
	origin := ""
	if draft.AgentToken != 0 {
		origin = fmt.Sprintf(" It was written by an agent (token %q); the signers should know that.", draft.AgentLabel)
	}
	return r.reply(ctx, fmt.Sprintf(
		"Draft #%d is now proposal #%d, from %s: %d payment(s), %s in total.%s\n\n"+
			"It needs signing weight %d and has %d. Sign it here:\n%s\n\n"+
			"Everyone else signing runs /approve %d to get their own link. The page adds "+
			"the total up itself from the envelope; check it against this one.",
		draft.ID, view.Proposal.ID, treasuryName(treasury), len(draft.Rows), draft.Total.Display(), origin,
		view.Need, view.Have, link, view.Proposal.ID))
}

// discard drops a draft.
func (s *Service) discard(ctx context.Context, r *request) error {
	id, ok := draftNumber(r.msg)
	if !ok {
		return r.reply(ctx, "Which draft? Try: /discard 3 — /drafts lists them.")
	}
	err := s.store.ResolveDraft(ctx, r.org.ID, id, "discarded", 0)
	switch {
	case errors.Is(err, store.ErrNoDraft):
		return r.reply(ctx, fmt.Sprintf("There's no open draft #%d here.", id))
	case err != nil:
		return err
	}
	return r.reply(ctx, fmt.Sprintf("Draft #%d is discarded. Nothing was proposed.", id))
}

// connectorCommands are registered alongside the rest.
func (s *Service) connectorCommands() []*command {
	opt := func(name, description string, required bool) chat.Option {
		return chat.Option{Name: name, Description: description, Type: chat.OptString, Required: required}
	}
	return []*command{
		{
			name:        "connector",
			needs:       needsConnectors,
			description: "Add, set up, grant, revoke or remove a connector",
			options: []chat.Option{
				opt("action", "add, setup, grant, revoke or remove", true),
				opt("name", "What this workspace calls it", true),
				opt("kind", "When adding: sheets, mcp or webhook", false),
				opt("per-hour", "When granting: calls an hour", false),
			},
			minRole: chat.RoleAdmin,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.connectorCmd(ctx, r) },
		},
		{
			name:        "connectors",
			needs:       needsConnectors,
			description: "List this workspace's connectors",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.connectors(ctx, r) },
		},
		{
			name:        "read",
			needs:       needsConnectors,
			description: "Show what a connector returns",
			options: []chat.Option{
				opt("connector", "Its name here", true),
				opt("what", "A sheet and range, or a tool and its arguments", false),
			},
			minRole: chat.RoleObserver,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.read(ctx, r) },
		},
		{
			name:        "payroll",
			needs:       needsConnectors,
			description: "Read a payroll from a connector, as a draft",
			options: []chat.Option{
				opt("connector", "Its name here", true),
				opt("what", "A sheet and range, or a tool and its arguments", false),
				opt("memo", "A note that goes on chain", false),
			},
			minRole: chat.RoleProposer,
			run:     func(ctx context.Context, s *Service, r *request) error { return s.payroll(ctx, r) },
		},
		{
			name:        "drafts",
			description: "List recent draft payrolls",
			minRole:     chat.RoleObserver,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.drafts(ctx, r) },
		},
		{
			name:        "propose",
			description: "Put a draft payroll to the treasury's signers",
			options:     []chat.Option{opt("draft", "The draft's number", true)},
			minRole:     chat.RoleProposer,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.propose(ctx, r) },
		},
		{
			name:        "discard",
			description: "Drop a draft payroll",
			options:     []chat.Option{opt("draft", "The draft's number", true)},
			minRole:     chat.RoleProposer,
			run:         func(ctx context.Context, s *Service, r *request) error { return s.discard(ctx, r) },
		},
	}
}
