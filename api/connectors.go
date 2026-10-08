package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/egress"
	"github.com/stelfin/stelfin/connector/mcpclient"
	"github.com/stelfin/stelfin/connector/sheets"
	"github.com/stelfin/stelfin/connector/webhook"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/mcp"
)

// Connectors: the tools a workspace already uses, reached from here.
//
// Three things are kept apart in this file, because confusing any two of them
// is how a connector comes to do something nobody agreed to.
//
// Registering a connector says where it is. It grants nothing, and an admin can
// do it alone.
//
// Granting one says stelfin may call it, for what, and how often. That is a
// statement in plain text, and it is put in force by the treasury's signers
// signing a challenge that commits to its hash — at the same weight a payment
// takes. No chat role can stand in for those signatures, and the signed
// envelope is kept with the grant so that anyone can check it later without
// taking this deployment's word.
//
// Using one goes through connector.Guard, every time, and the guard reads only
// grants that were signed. What comes back is text. A sheet can produce a draft
// payroll; it cannot produce a payment, because a draft becomes a proposal only
// when a person says so, and a proposal becomes a payment only when the
// treasury's signers sign that too.

var (
	// ErrConnectorsUnavailable reports a deployment that has not been
	// configured for the kind of connector that was asked for.
	ErrConnectorsUnavailable = errors.New("api: that kind of connector is not available on this deployment")

	// ErrConnectorName reports a name that cannot be used for a connector.
	ErrConnectorName = errors.New("api: a connector's name is lower-case letters, digits and hyphens")

	// ErrConnectorKind reports a kind this build does not know.
	ErrConnectorKind = errors.New("api: a connector is sheets, mcp or webhook")

	// ErrConnectorEndpoint reports an endpoint that cannot be registered.
	ErrConnectorEndpoint = errors.New("api: that endpoint cannot be used")

	// ErrConnectorNotReady reports a connector that has not been set up yet.
	ErrConnectorNotReady = errors.New("api: that connector has not been set up yet")

	// ErrConnectorChanged reports a grant request that no longer describes the
	// connector it was made for.
	ErrConnectorChanged = errors.New("api: the connector has changed since this grant was requested")

	// ErrConnectorFailed reports a connector that was asked and did not answer
	// usefully. What it said, if anything, is deliberately not carried.
	ErrConnectorFailed = errors.New("api: the connector could not be read")

	// ErrMemoTooLong reports a memo a Stellar transaction cannot carry whole.
	ErrMemoTooLong = errors.New("api: that memo is too long")

	// ErrNotTabular reports rows that cannot be read as a payroll.
	ErrNotTabular = errors.New("api: those rows cannot be read as payments")
)

// ConnectorConfig is what a deployment needs to reach connectors at all.
type ConnectorConfig struct {
	// Sealer encrypts a connector's endpoint and credential at rest. Required
	// for any connector that has either, which is every kind but sheets.
	Sealer *connector.Sealer
	// Sheets supplies tokens for the Google Sheets API. Nil means no sheets.
	Sheets sheets.Tokens
	// SheetsHTTP is the client the Sheets API is called with. Nil uses the
	// reader's own. The Sheets endpoint is a constant and Google's, so this is
	// not a way to point it somewhere else — only to supply a transport.
	SheetsHTTP *http.Client
	// SheetsAccount is the address a workspace shares its spreadsheet with.
	// Shown in replies; it is not a secret.
	SheetsAccount string
	// Egress is the only way an endpoint a workspace supplied is ever dialled.
	// Required for mcp and webhook.
	Egress *egress.Client
}

var connectorName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// DefaultGrantRate is how often a connector may be called when a grant does
// not say. Generous for a person reading a sheet, and far below anything that
// would hurt whoever is on the other end.
const DefaultGrantRate = 60

// grants adapts the store to connector.Grants.
//
// The guard asks by the connector's id, which is the name the workspace gave
// it. The store answers only with grants that carry signatures: a request that
// was never signed is not found, which the guard reads as "not granted".
type grants struct{ store *store.Store }

func (g grants) GrantFor(ctx context.Context, org int64, id string) (connector.Grant, error) {
	grant, err := g.store.GrantInForce(ctx, ledger.OrgID(org), id)
	if errors.Is(err, store.ErrNoGrant) {
		return connector.Grant{}, fmt.Errorf("%w: %s", connector.ErrNotGranted, id)
	}
	if err != nil {
		return connector.Grant{}, err
	}
	return connector.Grant{
		Capabilities:  grant.Capabilities,
		SurfaceDigest: grant.SurfaceDigest,
		PerHour:       grant.PerHour,
	}, nil
}

// named presents a reader under the name its workspace registered it with.
//
// A reader describes itself by kind — every spreadsheet reader says "sheets" —
// and a grant is recorded against a name. Without this the guard would look a
// grant up under the kind, and two connectors of one kind in a workspace would
// share whichever grant was found first.
type named struct {
	connector.Reader
	name string
}

func (n named) Describe() connector.Descriptor {
	d := n.Reader.Describe()
	d.ID = n.name
	return d
}

// live is a constructed connector and the registration it was built from.
type live struct {
	reader named
	// stamp identifies the registration. A connector that has been set up
	// again is a different one, and must not be served from a cached client
	// holding the old credential.
	stamp string
}

// liveConnectors caches constructed connectors.
//
// Kept, not rebuilt per call, because an MCP client carries a circuit breaker:
// it only protects anything if the same client sees consecutive failures.
type liveConnectors struct {
	mu sync.Mutex
	m  map[store.ConnectorID]live
}

func (s *Service) connectorsConfigured() error {
	if s.connectors == nil || s.guard == nil {
		return ErrConnectorsUnavailable
	}
	return nil
}

// RegisterConnector records that a workspace has a connector of this name and
// kind. It grants nothing, and it takes no endpoint.
//
// Where an MCP server or a webhook lives is given on the set-up page, with its
// credential, and never in chat. A webhook URL is very often its own password,
// and a command typed into a group is read by the group, kept by the platform
// and quoted back in replies.
func (s *Service) RegisterConnector(
	ctx context.Context, scope Scope, by store.MemberID, name, kind string,
) (store.Connector, error) {
	if err := scope.check(); err != nil {
		return store.Connector{}, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return store.Connector{}, err
	}
	if !connectorName.MatchString(name) {
		return store.Connector{}, ErrConnectorName
	}

	p := store.RegisterConnectorParams{Org: scope.Org, Name: name, Kind: kind, CreatedBy: by}
	switch kind {
	case store.ConnectorSheets:
		if s.connectors.Sheets == nil {
			return store.Connector{}, fmt.Errorf("%w: no Google service account is configured",
				ErrConnectorsUnavailable)
		}
		// Nothing more to supply: the endpoint is Google's and the credential
		// is the deployment's.
		p.Ready = true
	case store.ConnectorMCP, store.ConnectorWebhook:
		if s.connectors.Egress == nil || s.connectors.Sealer == nil {
			return store.Connector{}, fmt.Errorf("%w: no connector key is configured",
				ErrConnectorsUnavailable)
		}
	default:
		return store.Connector{}, ErrConnectorKind
	}
	return s.store.RegisterConnector(ctx, p)
}

// ConnectorSetup is what a connector's set-up established.
type ConnectorSetup struct {
	Connector store.Connector
	// Tools are what an MCP server offers, by name. Empty for other kinds.
	Tools []string
	// Rows is how many rows a webhook returned when it was tried.
	Rows int
}

// SetUpConnector takes a connector's credential, tries the connector, and
// records what it found.
//
// The credential arrives here from a page and never from chat: a chat message
// is read by a room, kept by a platform, and quoted back in replies. It is
// sealed before it is stored and is not returned by anything.
//
// The connector is tried before it is marked ready, so that what a grant later
// pins is what the endpoint really offers — an MCP server's actual tools — and
// so that a mistyped token is found by the person who typed it.
func (s *Service) SetUpConnector(
	ctx context.Context, scope Scope, id store.ConnectorID, endpoint, credential string,
) (*ConnectorSetup, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return nil, err
	}
	row, err := s.store.ConnectorByID(ctx, scope.Org, id)
	if err != nil {
		return nil, err
	}
	if row.Kind == store.ConnectorSheets {
		return nil, fmt.Errorf("%w: a spreadsheet connector has nothing to set up", ErrConnectorKind)
	}
	if len(credential) > 4096 {
		return nil, fmt.Errorf("%w: that credential is too long to be one", ErrConnectorEndpoint)
	}

	// Checked here for its shape, and again every time it is dialled for where
	// it actually leads. This one tells an admin about a typo; the other is the
	// one that stops a request reaching somewhere private.
	if err := s.connectors.Egress.CheckURL(endpoint); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnectorEndpoint, err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: it is not a URL", ErrConnectorEndpoint)
	}

	out := &ConnectorSetup{}
	var toolDigest string
	switch row.Kind {
	case store.ConnectorMCP:
		tools, digest, err := mcpclient.Discover(ctx, mcpclient.Config{
			ID: row.Name, Endpoint: endpoint,
			Credential: []byte(credential), HTTP: s.connectors.Egress,
		})
		if err != nil {
			return nil, connectorFailure(err)
		}
		if len(tools) == 0 {
			return nil, fmt.Errorf("%w: the server offers no tools", ErrConnectorFailed)
		}
		for _, t := range tools {
			out.Tools = append(out.Tools, t.Name)
		}
		sort.Strings(out.Tools)
		toolDigest = digest

	case store.ConnectorWebhook:
		hook, err := webhook.New(webhook.Config{
			ID: row.Name, URL: endpoint,
			Credential: []byte(credential), HTTP: s.connectors.Egress,
		})
		if err != nil {
			return nil, connectorFailure(err)
		}
		// Unguarded, deliberately and only here: there is no grant yet, and
		// this read is how the person setting it up learns whether the
		// endpoint answers. Its rows go nowhere but a count.
		seen, err := hook.Read(ctx, connector.Request{Limit: connector.MaxRows})
		if err != nil {
			return nil, connectorFailure(err)
		}
		out.Rows = len(seen.Rows)
	}

	done := store.ConnectorSetupParams{
		EndpointHost: parsed.Hostname(), EndpointDigest: digestOf(endpoint),
		Tools: out.Tools, ToolDigest: toolDigest,
	}
	if done.SealedEndpoint, err = s.connectors.Sealer.Seal(int64(row.Org), row.Name, []byte(endpoint)); err != nil {
		return nil, err
	}
	if credential != "" {
		if done.SealedCredential, err = s.connectors.Sealer.Seal(
			int64(row.Org), row.Name+"\x00credential", []byte(credential)); err != nil {
			return nil, err
		}
	}
	if err := s.store.CompleteConnectorSetup(ctx, scope.Org, id, done); err != nil {
		return nil, err
	}
	s.forget(id)

	if out.Connector, err = s.store.ConnectorByID(ctx, scope.Org, id); err != nil {
		return nil, err
	}
	return out, nil
}

// connectorFailure reports a connector that did not work, without what it said.
//
// Whatever a connector returns is somebody else's text. It can carry the URL,
// the credential echoed back, or a sentence written to be read by whoever sees
// the error — so the cause is kept for errors.Is and the wording is ours.
func connectorFailure(err error) error {
	switch {
	case errors.Is(err, egress.ErrForbiddenAddress):
		return fmt.Errorf("%w: its address is one stelfin will not connect to", ErrConnectorFailed)
	case errors.Is(err, mcpclient.ErrSurfaceChanged), errors.Is(err, connector.ErrSurfaceChanged):
		return fmt.Errorf("%w: %w", ErrConnectorFailed, connector.ErrSurfaceChanged)
	case errors.Is(err, mcpclient.ErrCircuitOpen):
		return fmt.Errorf("%w: it has failed repeatedly and is being left alone for a minute", ErrConnectorFailed)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: it did not answer in time", ErrConnectorFailed)
	default:
		return fmt.Errorf("%w: %w", ErrConnectorFailed, err)
	}
}

// reader constructs, or returns the cached, connector for a registration.
func (s *Service) reader(row store.Connector) (named, error) {
	if !row.Ready {
		return named{}, fmt.Errorf("%w: %s", ErrConnectorNotReady, row.Name)
	}
	stamp := row.SealedEndpoint + "\x00" + row.SealedCredential + "\x00" + row.ToolDigest

	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	if cached, ok := s.live.m[row.ID]; ok && cached.stamp == stamp {
		return cached.reader, nil
	}

	var built connector.Reader
	switch row.Kind {
	case store.ConnectorSheets:
		if s.connectors.Sheets == nil {
			return named{}, ErrConnectorsUnavailable
		}
		r, err := sheets.New(int64(row.Org), s.connectors.Sheets, s.connectors.SheetsHTTP)
		if err != nil {
			return named{}, err
		}
		built = r

	case store.ConnectorMCP, store.ConnectorWebhook:
		endpoint, err := s.connectors.Sealer.Open(int64(row.Org), row.Name, row.SealedEndpoint)
		if err != nil {
			return named{}, err
		}
		var credential []byte
		if row.SealedCredential != "" {
			if credential, err = s.connectors.Sealer.Open(
				int64(row.Org), row.Name+"\x00credential", row.SealedCredential); err != nil {
				return named{}, err
			}
		}
		if row.Kind == store.ConnectorMCP {
			c, err := mcpclient.New(mcpclient.Config{
				ID: row.Name, Endpoint: string(endpoint),
				SurfaceDigest: row.ToolDigest, Tools: row.Tools,
				Credential: credential, HTTP: s.connectors.Egress,
			})
			if err != nil {
				return named{}, err
			}
			built = c
		} else {
			h, err := webhook.New(webhook.Config{
				ID: row.Name, URL: string(endpoint), Credential: credential, HTTP: s.connectors.Egress,
			})
			if err != nil {
				return named{}, err
			}
			built = h
		}

	default:
		return named{}, ErrConnectorKind
	}

	// Checked at construction and not trusted from the type: a connector that
	// both reads and proposes is its own confused deputy.
	if err := connector.CheckKind(built); err != nil {
		return named{}, err
	}
	out := named{Reader: built, name: row.Name}
	if s.live.m == nil {
		s.live.m = make(map[store.ConnectorID]live)
	}
	s.live.m[row.ID] = live{reader: out, stamp: stamp}
	return out, nil
}

func (s *Service) forget(id store.ConnectorID) {
	s.live.mu.Lock()
	delete(s.live.m, id)
	s.live.mu.Unlock()
}

// capabilityFor names the capability one read needs.
func capabilityFor(row store.Connector, selector string) (string, error) {
	switch row.Kind {
	case store.ConnectorSheets:
		return sheets.CapabilityReadRange, nil
	case store.ConnectorWebhook:
		return webhook.CapabilityReadRows, nil
	case store.ConnectorMCP:
		tool, _, err := mcpclient.ParseSelector(selector)
		if err != nil {
			return "", err
		}
		return mcpclient.Capability(tool), nil
	default:
		return "", ErrConnectorKind
	}
}

// GrantRequest is a grant put to a treasury's signers.
type GrantRequest struct {
	Challenge *LinkChallenge
	Connector store.Connector
	Treasury  store.Treasury
	Statement string
	PerHour   int
}

// grantStatement is the text a treasury's signers agree to.
//
// Plain, line by line, tab-separated — so that it can be shown to a person as
// it is, hashed by a browser as it is, and compared by eye with what a wallet
// displays. Every fact a signer is relying on is a line here, and the digest of
// these exact bytes is what their signature commits to.
//
// The endpoint appears as a host and a digest, never in full: a webhook URL is
// often its own password, and this text is shown to every signer and kept. The
// digest still binds the grant to the one endpoint it was given for.
func grantStatement(
	org store.Org, treasury store.Treasury, row store.Connector, d connector.Descriptor, perHour int,
) string {
	capabilities := append([]string(nil), d.Capabilities...)
	sort.Strings(capabilities)

	where := row.EndpointHost
	if row.Kind == store.ConnectorSheets {
		where = "Google Sheets"
	}

	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%s\t%s\n", key, value) }
	b.WriteString("stelfin-connector-grant-1\n")
	line("workspace", fmt.Sprintf("%d %s", org.ID, oneLine(org.DisplayName)))
	line("network", org.Network)
	line("treasury", treasury.Address)
	line("connector", row.Name)
	line("kind", row.Kind)
	line("reaches", where)
	line("endpoint_digest", row.EndpointDigest)
	line("surface", connector.SurfaceDigest(d))
	for _, c := range capabilities {
		line("may", c)
	}
	line("rate", fmt.Sprintf("%d calls an hour", perHour))
	line("cannot", "sign, submit or move funds; anything it returns is text a person must act on")
	return b.String()
}

// oneLine keeps a user-chosen name from adding lines to a statement.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// PrepareGrant puts a connector's grant to a treasury's signers.
func (s *Service) PrepareGrant(
	ctx context.Context, scope Scope, identity store.IdentityID, by store.MemberID,
	name string, treasuryID store.TreasuryID, perHour int,
) (*GrantRequest, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if perHour == 0 {
		perHour = DefaultGrantRate
	}
	if perHour < 1 || perHour > 3600 {
		return nil, errors.New("api: a grant's rate is between 1 and 3600 calls an hour")
	}

	row, err := s.store.ConnectorByName(ctx, scope.Org, name)
	if err != nil {
		return nil, err
	}
	reader, err := s.reader(row)
	if err != nil {
		return nil, err
	}
	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, treasuryID)
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryClassic {
		return nil, fmt.Errorf("api: %s is a contract account; its authorisation is not signatures",
			treasury.Address)
	}

	descriptor := reader.Describe()
	statement := grantStatement(org, treasury, row, descriptor, perHour)
	digest := digestOf(statement)

	challenge, err := s.challenges.BuildStatement(treasury.Address, digest)
	if err != nil {
		return nil, err
	}
	if err := s.store.SaveChallenge(ctx, store.Challenge{
		Hash: challenge.Hash, Org: scope.Org, Identity: identity,
		Purpose: store.PurposeGrantConnector, Address: challenge.Address,
		XDR: challenge.XDR, ExpiresAt: challenge.ExpiresAt,
	}); err != nil {
		return nil, err
	}
	if err := s.store.RequestGrant(ctx, store.RequestGrantParams{
		Org: scope.Org, Connector: row.ID, Treasury: treasury.ID,
		Capabilities: descriptor.Capabilities, SurfaceDigest: connector.SurfaceDigest(descriptor),
		PerHour: perHour, Statement: statement, StatementDigest: digest,
		ChallengeHash: challenge.Hash, RequestedBy: by,
	}); err != nil {
		return nil, err
	}

	return &GrantRequest{
		Challenge: &LinkChallenge{
			Address: challenge.Address, XDR: challenge.XDR, Hash: challenge.Hash,
			NetworkPassphrase: s.challenges.Network(), Purpose: store.PurposeGrantConnector,
			Statement: statement,
		},
		Connector: row, Treasury: treasury, Statement: statement, PerHour: perHour,
	}, nil
}

// GrantResult describes a grant that is now in force.
type GrantResult struct {
	Connector store.Connector
	Grant     store.ConnectorGrant
	Treasury  store.Treasury
	Threshold int32
}

// SubmitGrant checks a signed statement against the treasury's own signers and
// puts the grant in force.
//
// The signer set is read from the network now, as it is for a payment. And the
// statement is rebuilt from the connector as it stands now and compared with
// the one that was signed: if the connector was set up again while signatures
// were being collected, what was agreed to describes something that no longer
// exists, and it is refused rather than applied to its replacement.
func (s *Service) SubmitGrant(
	ctx context.Context, scope Scope, hash, signedXDR string,
) (*GrantResult, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if signedXDR == "" {
		return nil, errors.New("api: a signed challenge is required")
	}

	c, err := s.store.LiveChallenge(ctx, scope.Org, hash)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}
	if c.Purpose != store.PurposeGrantConnector {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	request, err := s.store.GrantRequest(ctx, scope.Org, hash)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}

	row, err := s.store.ConnectorByID(ctx, scope.Org, request.Connector)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, request.Treasury)
	if err != nil {
		return nil, err
	}
	if treasury.Address != c.Address {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	reader, err := s.reader(row)
	if err != nil {
		return nil, err
	}
	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	if now := grantStatement(org, treasury, row, reader.Describe(), request.PerHour); now != request.Statement {
		return nil, fmt.Errorf("%w: %s", ErrConnectorChanged, row.Name)
	}

	set, err := s.settle.SignerSetOf(ctx, c.Address)
	if err != nil {
		return nil, err
	}
	summary := set.Summary(c.Address)
	if len(summary) == 0 {
		return nil, fmt.Errorf("%w: %s has no key signers", ErrTreasuryUnprovable, c.Address)
	}
	signed, err := s.challenges.VerifyStatement(
		signedXDR, txnbuild.Threshold(set.Medium), summary, request.StatementDigest)
	if err != nil {
		return nil, err
	}
	sort.Strings(signed)

	grant, err := s.store.ConsumeGrantChallenge(ctx, scope.Org, hash, c.Address, signedXDR, signed)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}
	return &GrantResult{Connector: row, Grant: grant, Treasury: treasury, Threshold: int32(set.Medium)}, nil
}

// ReadConnector reads through the guard.
//
// The only door. Everything that reaches a connector on a workspace's behalf
// comes through here, and so through a signed grant, the pinned surface and
// the rate limit.
func (s *Service) ReadConnector(
	ctx context.Context, scope Scope, name, selector string,
) (connector.Observation, store.Connector, error) {
	if err := scope.check(); err != nil {
		return connector.Observation{}, store.Connector{}, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return connector.Observation{}, store.Connector{}, err
	}
	row, err := s.store.ConnectorByName(ctx, scope.Org, name)
	if err != nil {
		return connector.Observation{}, store.Connector{}, err
	}
	reader, err := s.reader(row)
	if err != nil {
		return connector.Observation{}, row, err
	}
	capability, err := capabilityFor(row, selector)
	if err != nil {
		return connector.Observation{}, row, fmt.Errorf("%w: %v", ErrConnectorFailed, err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	seen, err := s.guard.Read(ctx, int64(scope.Org), reader, capability, connector.Request{Selector: selector})
	if err != nil {
		switch {
		case errors.Is(err, connector.ErrNotGranted), errors.Is(err, connector.ErrSurfaceChanged),
			errors.Is(err, connector.ErrCapabilityNotGranted), errors.Is(err, connector.ErrRateExceeded):
			// The guard's own refusals are ours, and say what to do.
			return connector.Observation{}, row, err
		}
		return connector.Observation{}, row, connectorFailure(err)
	}
	return seen, row, nil
}

// draftFromRows reads a table as a payroll: who, how much, and a note.
//
// The columns are found by their headings where there are headings, and by
// position where there are not: destination, amount, memo. A heading row is
// recognised only when it names both a destination and an amount, so a table
// with headings this does not know is refused rather than paid by position —
// a sheet whose first two columns are "employee id" and "start date" must not
// be read as addresses and amounts.
//
// Nothing is interpreted here. Every cell goes on as untrusted text, and the
// amounts and addresses are read once, by the code that reads them for every
// other payment.
func draftFromRows(seen connector.Observation, assetCode string) (connector.Draft, error) {
	// The deployment's own asset names an amount column, whatever it is: a
	// sheet headed "EURC" is as ordinary on a EURC deployment as "USDC" is on
	// this one.
	asset := strings.ToLower(strings.TrimSpace(assetCode))
	header := seen.Columns
	rows := seen.Rows
	firstLine := 1
	if len(header) == 0 && len(rows) > 0 && looksLikeHeader(rows[0], asset) {
		header, rows, firstLine = rows[0], rows[1:], 2
	}

	to, amount, memo := 0, 1, 2
	if len(header) > 0 {
		to, amount, memo = -1, -1, -1
		for i, cell := range header {
			switch strings.ToLower(strings.TrimSpace(cell.Unwrap())) {
			case "to", "address", "destination", "recipient", "wallet", "account":
				if to == -1 {
					to = i
				}
			case "amount", "pay", "payment", "total", asset:
				if amount == -1 {
					amount = i
				}
			case "memo", "note", "notes", "reference", "for":
				if memo == -1 {
					memo = i
				}
			}
		}
		if to == -1 || amount == -1 {
			return connector.Draft{}, fmt.Errorf(
				"%w: the headings name no destination and amount. "+
					"Use columns headed \"address\" and \"amount\", or no headings at all",
				ErrNotTabular)
		}
	}

	draft := connector.Draft{ReadAt: seen.ReadAt, Truncated: seen.Truncated}
	for i, row := range rows {
		cell := func(at int) connector.Untrusted[string] {
			if at < 0 || at >= len(row) {
				return connector.Wrap("")
			}
			return row[at]
		}
		// A wholly empty line is the bottom of a sheet, not a payment of
		// nothing to nobody.
		if strings.TrimSpace(cell(to).Unwrap()) == "" && strings.TrimSpace(cell(amount).Unwrap()) == "" {
			continue
		}
		draft.Rows = append(draft.Rows, connector.DraftRow{
			Line:            firstLine + i,
			AmountText:      cell(amount),
			DestinationText: cell(to),
			MemoText:        cell(memo),
		})
	}
	if len(draft.Rows) == 0 {
		return connector.Draft{}, ErrBatchEmpty
	}
	return draft, nil
}

// looksLikeHeader reports a first row that names columns and is not a payment.
func looksLikeHeader(row []connector.Untrusted[string], asset string) bool {
	named := 0
	for _, cell := range row {
		switch strings.ToLower(strings.TrimSpace(cell.Unwrap())) {
		case "to", "address", "destination", "recipient", "wallet", "account",
			"amount", "pay", "payment", "total", asset:
			named++
		}
	}
	return named >= 2
}

// DraftPayroll reads a connector and keeps what it found as a draft.
//
// Every row has to be readable, or there is no draft: a payroll with one row
// quietly dropped pays everyone but one person, and nobody finds out from the
// total. The error names each row that failed and why.
func (s *Service) DraftPayroll(
	ctx context.Context, scope Scope, by store.MemberID, name, selector, memo string,
) (store.Draft, error) {
	seen, row, err := s.ReadConnector(ctx, scope, name, selector)
	if err != nil {
		return store.Draft{}, err
	}
	if seen.Truncated {
		return store.Draft{}, fmt.Errorf(
			"%w: there are more rows than were read, and a payroll cut short pays some people and not others",
			ErrBatchTooLarge)
	}
	draft, err := draftFromRows(seen, s.cfg.AssetCode)
	if err != nil {
		return store.Draft{}, err
	}
	batch, err := s.ResolveBatch(ctx, scope, draft)
	if err != nil {
		return store.Draft{}, err
	}
	// Refused here and not shortened later. A memo is the one line of this
	// payroll that every recipient sees on chain, and silently cutting it is
	// how "March salaries, less the advance" becomes "March salaries, less th".
	if len(memo) > 28 {
		return store.Draft{}, fmt.Errorf("%w: a memo holds 28 bytes and that is %d", ErrMemoTooLong, len(memo))
	}

	out := store.Draft{
		Org: scope.Org, Connector: row.ID, Selector: selector, Memo: memo,
		Total: batch.Total, CreatedBy: by,
	}
	for _, r := range batch.Rows {
		out.Rows = append(out.Rows, store.DraftRow{
			Line: r.Line, Amount: r.Amount.String(), Destination: r.Destination,
			Label: r.DestinationLabel, Memo: r.Memo,
			SaidAmount: r.SaidAmount, SaidDestination: r.SaidDestination,
		})
	}
	return s.store.SaveDraft(ctx, out)
}

// batchOf turns a stored draft back into the batch it was resolved to.
//
// From the stored reading, not from the sheet again and not from the text
// again. What a second person proposes has to be what the first person
// reviewed: the sheet may have changed since, and a saved name means a
// different address to a different member.
func batchOf(d store.Draft) (*Batch, error) {
	batch := &Batch{}
	for _, r := range d.Rows {
		amount, err := money.Parse(r.Amount)
		if err != nil || amount <= 0 {
			return nil, fmt.Errorf("api: draft %d row %d holds an unreadable amount", d.ID, r.Line)
		}
		batch.Rows = append(batch.Rows, BatchRow{
			Line: r.Line, Amount: amount, Destination: r.Destination,
			DestinationLabel: r.Label, Memo: r.Memo,
			SaidAmount: r.SaidAmount, SaidDestination: r.SaidDestination,
		})
		batch.Total += amount
	}
	if batch.Total != d.Total {
		return nil, fmt.Errorf("api: draft %d no longer adds up to its recorded total", d.ID)
	}
	return batch, nil
}

// ProposeDraft puts a reviewed draft to the treasury's signers.
func (s *Service) ProposeDraft(
	ctx context.Context, scope Scope, id store.DraftID, treasury store.TreasuryID, by store.MemberID,
) (*ProposalView, store.Draft, error) {
	if err := scope.check(); err != nil {
		return nil, store.Draft{}, err
	}
	draft, err := s.store.OpenDraft(ctx, scope.Org, id)
	if err != nil {
		return nil, store.Draft{}, err
	}
	batch, err := batchOf(draft)
	if err != nil {
		return nil, draft, err
	}
	view, err := s.proposeBatch(ctx, scope, batch, treasury, draft.Memo, by)
	if err != nil {
		return nil, draft, err
	}
	// The proposal exists. If recording that on the draft fails, the draft is
	// left open, which is recoverable: proposing it again is refused by the
	// treasury's one-open-proposal rule rather than paid twice.
	if err := s.store.ResolveDraft(ctx, scope.Org, id, "proposed", view.Proposal.ID); err != nil {
		return view, draft, err
	}
	return view, draft, nil
}

// SetupPrefix marks a link token that leads to a connector's set-up and not to
// a challenge. The rest of the value is the connector's id.
const SetupPrefix = "setup:"

// SetupToken is the value a set-up link's token carries for a connector.
func SetupToken(id store.ConnectorID) string { return fmt.Sprintf("%s%d", SetupPrefix, id) }

// setupTarget reads a set-up token's value back into a connector id.
func setupTarget(value string) (store.ConnectorID, bool) {
	rest, ok := strings.CutPrefix(value, SetupPrefix)
	if !ok {
		return 0, false
	}
	var id int64
	if _, err := fmt.Sscanf(rest, "%d", &id); err != nil || id <= 0 || fmt.Sprint(id) != rest {
		return 0, false
	}
	return store.ConnectorID(id), true
}

// ConnectorForSetup returns the connector a set-up link names, for the page to
// show what is being set up before anything is typed into it.
func (s *Service) ConnectorForSetup(ctx context.Context, scope Scope, id store.ConnectorID) (store.Connector, error) {
	if err := scope.check(); err != nil {
		return store.Connector{}, err
	}
	if err := s.connectorsConfigured(); err != nil {
		return store.Connector{}, err
	}
	return s.store.ConnectorByID(ctx, scope.Org, id)
}

// SheetsAccount is the address a workspace shares a spreadsheet with so that
// this deployment can read it. Empty when no service account is configured.
func (s *Service) SheetsAccount() string {
	if s.connectors == nil {
		return ""
	}
	return s.connectors.SheetsAccount
}

// sharedLimits is the guard's rate limit, counted in the database so that
// every instance of this deployment draws on one allowance.
type sharedLimits struct{ store *store.Store }

func (l sharedLimits) Allow(ctx context.Context, org int64, connector string, perHour int, now time.Time) (bool, error) {
	return l.store.AllowConnectorCall(ctx, ledger.OrgID(org), connector, perHour, now)
}

// DraftForAgent writes a draft payroll on behalf of an agent holding a
// propose-tier token. It implements mcp.Drafter.
//
// The rows go through the same reading as a member's sheet — the same amount
// parser, the same refusal of a batch with any unreadable row — with one more
// rule: every destination is an address. A saved name belongs to the member
// who saved it, and an agent is not one.
func (s *Service) DraftForAgent(
	ctx context.Context, org ledger.OrgID, token int64, lines []mcp.PaymentLine, memo string,
) (mcp.DraftSummary, error) {
	if token <= 0 {
		return mcp.DraftSummary{}, errors.New("api: an agent's draft names the token that made it")
	}
	if len(memo) > 28 {
		return mcp.DraftSummary{}, &mcp.InvalidArgument{
			Message: fmt.Sprintf("the memo is %d bytes and a Stellar memo holds 28", len(memo)),
		}
	}
	draft := connector.Draft{ReadAt: time.Now()}
	for i, line := range lines {
		if !strkey.IsValidEd25519PublicKey(strings.TrimSpace(line.To)) {
			return mcp.DraftSummary{}, &mcp.InvalidArgument{
				Message: fmt.Sprintf("payment %d: %q is not a Stellar account address", i+1, line.To),
			}
		}
		draft.Rows = append(draft.Rows, connector.DraftRow{
			Line:            i + 1,
			AmountText:      connector.Wrap(line.Amount),
			DestinationText: connector.Wrap(strings.TrimSpace(line.To)),
			MemoText:        connector.Wrap(line.Memo),
		})
	}

	// An owner that saved nothing, so no label can resolve: the address check
	// above is the rule, and this makes it the only way through as well.
	scope := Scope{Org: org, OwnerRef: fmt.Sprintf("mcp:%d", token)}
	batch, err := s.ResolveBatch(ctx, scope, draft)
	var unreadable *BatchError
	switch {
	case errors.As(err, &unreadable):
		return mcp.DraftSummary{}, &mcp.InvalidArgument{Message: unreadable.Error()}
	case errors.Is(err, ErrBatchEmpty), errors.Is(err, ErrBatchTooLarge), errors.Is(err, ErrBatchUnreadable):
		return mcp.DraftSummary{}, &mcp.InvalidArgument{Message: afterPrefix(err)}
	case err != nil:
		return mcp.DraftSummary{}, err
	}

	out := store.Draft{Org: org, Memo: memo, Total: batch.Total, AgentToken: token}
	for _, r := range batch.Rows {
		out.Rows = append(out.Rows, store.DraftRow{
			Line: r.Line, Amount: r.Amount.String(), Destination: r.Destination, Memo: r.Memo,
			SaidAmount: r.SaidAmount, SaidDestination: r.SaidDestination,
		})
	}
	saved, err := s.store.SaveDraft(ctx, out)
	if err != nil {
		return mcp.DraftSummary{}, err
	}
	return mcp.DraftSummary{
		ID: int64(saved.ID), Rows: len(saved.Rows), Total: saved.Total.String(),
		Note: "This is a draft. A member of the workspace has to propose it, and the treasury's " +
			"signers have to sign it, before anything is paid.",
	}, nil
}

// afterPrefix drops the package prefix from one of our own errors.
func afterPrefix(err error) string {
	if _, rest, ok := strings.Cut(err.Error(), ": "); ok {
		return rest
	}
	return err.Error()
}
