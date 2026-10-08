package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// The kinds of connector a workspace can register.
const (
	ConnectorSheets  = "sheets"
	ConnectorMCP     = "mcp"
	ConnectorWebhook = "webhook"
)

// PurposeGrantConnector proves a treasury's signers authorised a connector.
const PurposeGrantConnector = "grant_connector"

var (
	// ErrNoConnector reports a connector this workspace has not registered.
	ErrNoConnector = errors.New("store: no such connector")
	// ErrConnectorExists reports a name already in use here.
	ErrConnectorExists = errors.New("store: a connector with that name already exists")
	// ErrNoGrant reports a connector with no grant in force.
	ErrNoGrant = errors.New("store: that connector has no grant in force")
	// ErrNoDraft reports a draft that does not exist here, or is no longer open.
	ErrNoDraft = errors.New("store: no such open draft")
)

// ConnectorID identifies a registered connector.
type ConnectorID int64

// Connector is one registered connector.
type Connector struct {
	ID   ConnectorID
	Org  ledger.OrgID
	Name string
	Kind string

	// SealedEndpoint and SealedCredential are ciphertext; this package cannot
	// read either, which is the point of keeping them sealed at this layer.
	SealedEndpoint   string
	EndpointHost     string
	EndpointDigest   string
	SealedCredential string

	Tools      []string
	ToolDigest string

	// Ready reports that whatever the kind needs has been supplied and checked.
	Ready     bool
	CreatedAt time.Time
}

const connectorColumns = `id, org_id, name, kind, sealed_endpoint, endpoint_host,
	endpoint_digest, sealed_credential, tools, tool_digest, ready_at IS NOT NULL, created_at`

func scanConnector(row pgx.Row) (Connector, error) {
	var c Connector
	err := row.Scan(&c.ID, &c.Org, &c.Name, &c.Kind, &c.SealedEndpoint, &c.EndpointHost,
		&c.EndpointDigest, &c.SealedCredential, &c.Tools, &c.ToolDigest, &c.Ready, &c.CreatedAt)
	return c, err
}

// RegisterConnectorParams describes a connector being registered.
type RegisterConnectorParams struct {
	Org        ledger.OrgID
	Name, Kind string
	// Ready is true for a kind that needs nothing more — a spreadsheet, whose
	// endpoint and credential are the deployment's own.
	Ready     bool
	CreatedBy MemberID
}

// RegisterConnector records a connector. It grants nothing.
func (s *Store) RegisterConnector(ctx context.Context, p RegisterConnectorParams) (Connector, error) {
	c, err := scanConnector(s.pool.QueryRow(ctx, `
		INSERT INTO connectors (org_id, name, kind, ready_at, created_by)
		VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END, $5)
		RETURNING `+connectorColumns,
		int64(p.Org), p.Name, p.Kind, p.Ready, int64(p.CreatedBy)))
	if isUniqueViolation(err) {
		return Connector{}, fmt.Errorf("%w: %s", ErrConnectorExists, p.Name)
	}
	if err != nil {
		return Connector{}, fmt.Errorf("store: register connector: %w", err)
	}
	return c, nil
}

// ConnectorByName returns a workspace's live connector of that name.
func (s *Store) ConnectorByName(ctx context.Context, org ledger.OrgID, name string) (Connector, error) {
	c, err := scanConnector(s.pool.QueryRow(ctx, `
		SELECT `+connectorColumns+` FROM connectors
		 WHERE org_id = $1 AND name = $2 AND removed_at IS NULL`, int64(org), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Connector{}, fmt.Errorf("%w: %s", ErrNoConnector, name)
	}
	if err != nil {
		return Connector{}, fmt.Errorf("store: connector %s: %w", name, err)
	}
	return c, nil
}

// ConnectorByID returns a workspace's live connector.
func (s *Store) ConnectorByID(ctx context.Context, org ledger.OrgID, id ConnectorID) (Connector, error) {
	c, err := scanConnector(s.pool.QueryRow(ctx, `
		SELECT `+connectorColumns+` FROM connectors
		 WHERE org_id = $1 AND id = $2 AND removed_at IS NULL`, int64(org), int64(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Connector{}, fmt.Errorf("%w: %d", ErrNoConnector, id)
	}
	if err != nil {
		return Connector{}, fmt.Errorf("store: connector %d: %w", id, err)
	}
	return c, nil
}

// Connectors lists a workspace's live connectors, by name.
func (s *Store) Connectors(ctx context.Context, org ledger.OrgID) ([]Connector, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+connectorColumns+` FROM connectors
		 WHERE org_id = $1 AND removed_at IS NULL ORDER BY name`, int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: list connectors: %w", err)
	}
	defer rows.Close()

	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan connector: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConnectorSetupParams is what a connector's set-up established.
type ConnectorSetupParams struct {
	SealedEndpoint   string
	EndpointHost     string
	EndpointDigest   string
	SealedCredential string
	// Tools and ToolDigest are an MCP server's surface; empty otherwise.
	Tools      []string
	ToolDigest string
}

// CompleteConnectorSetup records where a connector is, its sealed credential if
// it has one, and for an MCP server the tools it offers.
//
// Any grant in force is revoked in the same transaction. Set-up is where the
// endpoint, the credential and the tool surface are fixed, and a grant was
// given against the ones that were there before; carrying it over would let
// whoever can run set-up change what the signers authorised without asking
// them.
func (s *Store) CompleteConnectorSetup(
	ctx context.Context, org ledger.OrgID, id ConnectorID, p ConnectorSetupParams,
) error {
	if p.Tools == nil {
		p.Tools = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE connectors
		   SET sealed_endpoint = $3, endpoint_host = $4, endpoint_digest = $5,
		       sealed_credential = $6, tools = $7, tool_digest = $8, ready_at = now()
		 WHERE org_id = $1 AND id = $2 AND removed_at IS NULL AND kind <> 'sheets'`,
		int64(org), int64(id), p.SealedEndpoint, p.EndpointHost, p.EndpointDigest,
		p.SealedCredential, p.Tools, p.ToolDigest)
	if err != nil {
		return fmt.Errorf("store: complete connector setup: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrNoConnector, id)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE connector_grants SET revoked_at = now()
		 WHERE org_id = $1 AND connector_id = $2 AND granted_at IS NOT NULL AND revoked_at IS NULL`,
		int64(org), int64(id)); err != nil {
		return fmt.Errorf("store: revoke superseded grant: %w", err)
	}
	// And any request still collecting signatures: it describes the connector
	// as it was.
	if _, err := tx.Exec(ctx, `
		DELETE FROM connector_grants
		 WHERE org_id = $1 AND connector_id = $2 AND granted_at IS NULL`,
		int64(org), int64(id)); err != nil {
		return fmt.Errorf("store: drop superseded grant request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit connector setup: %w", err)
	}
	return nil
}

// RemoveConnector withdraws a connector and whatever grant it had.
func (s *Store) RemoveConnector(ctx context.Context, org ledger.OrgID, name string, by MemberID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE connectors SET removed_at = now()
		 WHERE org_id = $1 AND name = $2 AND removed_at IS NULL
		RETURNING id`, int64(org), name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNoConnector, name)
	}
	if err != nil {
		return fmt.Errorf("store: remove connector: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE connector_grants SET revoked_at = now(), revoked_by = $3
		 WHERE org_id = $1 AND connector_id = $2 AND granted_at IS NOT NULL AND revoked_at IS NULL`,
		int64(org), id, int64(by)); err != nil {
		return fmt.Errorf("store: revoke grant of removed connector: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit connector removal: %w", err)
	}
	return nil
}

// ConnectorGrant is what a treasury's signers authorised, and the proof.
type ConnectorGrant struct {
	ID        int64
	Org       ledger.OrgID
	Connector ConnectorID
	Treasury  TreasuryID

	Capabilities  []string
	SurfaceDigest string
	PerHour       int

	Statement       string
	StatementDigest string
	ChallengeHash   string
	SignedXDR       string
	SignedBy        []string

	GrantedAt *time.Time
}

const grantColumns = `id, org_id, connector_id, treasury_id, capabilities, surface_digest,
	per_hour, statement, statement_digest, challenge_hash, signed_xdr, signed_by, granted_at`

func scanGrant(row pgx.Row) (ConnectorGrant, error) {
	var g ConnectorGrant
	err := row.Scan(&g.ID, &g.Org, &g.Connector, &g.Treasury, &g.Capabilities, &g.SurfaceDigest,
		&g.PerHour, &g.Statement, &g.StatementDigest, &g.ChallengeHash, &g.SignedXDR,
		&g.SignedBy, &g.GrantedAt)
	return g, err
}

// RequestGrantParams describes a grant being put to a treasury's signers.
type RequestGrantParams struct {
	Org       ledger.OrgID
	Connector ConnectorID
	Treasury  TreasuryID

	Capabilities  []string
	SurfaceDigest string
	PerHour       int

	Statement       string
	StatementDigest string
	ChallengeHash   string
	RequestedBy     MemberID
}

// RequestGrant records a grant that is waiting for signatures.
//
// It is a request and confers nothing: GrantInForce reads only rows that have
// been signed. Earlier unsigned requests for the same connector are dropped, so
// asking again replaces the last ask rather than leaving two envelopes in
// circulation that say different things.
func (s *Store) RequestGrant(ctx context.Context, p RequestGrantParams) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM connector_grants
		 WHERE org_id = $1 AND connector_id = $2 AND granted_at IS NULL`,
		int64(p.Org), int64(p.Connector)); err != nil {
		return fmt.Errorf("store: supersede grant request: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO connector_grants (org_id, connector_id, treasury_id, capabilities,
			surface_digest, per_hour, statement, statement_digest, challenge_hash, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		int64(p.Org), int64(p.Connector), int64(p.Treasury), p.Capabilities,
		p.SurfaceDigest, p.PerHour, p.Statement, p.StatementDigest, p.ChallengeHash,
		int64(p.RequestedBy)); err != nil {
		return fmt.Errorf("store: request grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit grant request: %w", err)
	}
	return nil
}

// GrantRequest returns the grant waiting on a challenge.
func (s *Store) GrantRequest(ctx context.Context, org ledger.OrgID, challengeHash string) (ConnectorGrant, error) {
	g, err := scanGrant(s.pool.QueryRow(ctx, `
		SELECT `+grantColumns+` FROM connector_grants
		 WHERE org_id = $1 AND challenge_hash = $2 AND granted_at IS NULL AND revoked_at IS NULL`,
		int64(org), challengeHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectorGrant{}, fmt.Errorf("%w: %s", ErrNoChallenge, challengeHash)
	}
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: grant request: %w", err)
	}
	return g, nil
}

// ConsumeGrantChallenge spends a signed challenge and puts its grant in force.
//
// One transaction: the challenge is claimed, any grant already in force for the
// connector is revoked, and this one takes its place. A challenge spent with no
// grant recorded would be signatures used up for nothing; two grants in force
// would be two answers to "what may this connector do".
//
// The caller has verified the signatures. What is stored here is the evidence
// of that — the signed envelope and who signed — so the check can be repeated
// by anyone, later, without this deployment.
func (s *Store) ConsumeGrantChallenge(
	ctx context.Context, org ledger.OrgID, challengeHash, address, signedXDR string, signedBy []string,
) (ConnectorGrant, error) {
	if signedXDR == "" || len(signedBy) == 0 {
		return ConnectorGrant{}, errors.New("store: a grant is put in force by signatures")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE auth_challenges
		   SET consumed_at = now()
		 WHERE hash = $1 AND org_id = $2 AND address = $3 AND purpose = $4
		   AND consumed_at IS NULL AND expires_at > now()`,
		challengeHash, int64(org), address, PurposeGrantConnector)
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: consume grant challenge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ConnectorGrant{}, fmt.Errorf("%w: %s", ErrNoChallenge, challengeHash)
	}

	var connector int64
	err = tx.QueryRow(ctx, `
		SELECT connector_id FROM connector_grants
		 WHERE org_id = $1 AND challenge_hash = $2 AND granted_at IS NULL AND revoked_at IS NULL
		 FOR UPDATE`, int64(org), challengeHash).Scan(&connector)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectorGrant{}, fmt.Errorf("%w: %s", ErrNoChallenge, challengeHash)
	}
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: find grant request: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE connector_grants SET revoked_at = now()
		 WHERE org_id = $1 AND connector_id = $2 AND granted_at IS NOT NULL AND revoked_at IS NULL`,
		int64(org), connector); err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: replace grant: %w", err)
	}

	g, err := scanGrant(tx.QueryRow(ctx, `
		UPDATE connector_grants
		   SET granted_at = now(), signed_xdr = $3, signed_by = $4
		 WHERE org_id = $1 AND challenge_hash = $2
		RETURNING `+grantColumns, int64(org), challengeHash, signedXDR, signedBy))
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: put grant in force: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: commit grant: %w", err)
	}
	return g, nil
}

// GrantInForce returns the signed grant for a workspace's connector, by name.
func (s *Store) GrantInForce(ctx context.Context, org ledger.OrgID, name string) (ConnectorGrant, error) {
	g, err := scanGrant(s.pool.QueryRow(ctx, `
		SELECT `+prefixed(grantColumns, "g")+`
		  FROM connector_grants g
		  JOIN connectors c ON c.id = g.connector_id AND c.org_id = g.org_id
		 WHERE g.org_id = $1 AND c.name = $2 AND c.removed_at IS NULL
		   AND g.granted_at IS NOT NULL AND g.revoked_at IS NULL`, int64(org), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectorGrant{}, fmt.Errorf("%w: %s", ErrNoGrant, name)
	}
	if err != nil {
		return ConnectorGrant{}, fmt.Errorf("store: grant for %s: %w", name, err)
	}
	return g, nil
}

// RevokeGrant withdraws a connector's grant.
//
// It takes no signatures, and that is not an oversight. Putting a grant in
// force adds something stelfin will do on a workspace's behalf, so the people
// who answer for the treasury decide. Withdrawing one only ever removes
// something, and the person who notices a connector misbehaving at two in the
// morning should not need a quorum to stop it.
func (s *Store) RevokeGrant(ctx context.Context, org ledger.OrgID, name string, by MemberID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE connector_grants g SET revoked_at = now(), revoked_by = $3
		  FROM connectors c
		 WHERE c.id = g.connector_id AND c.org_id = g.org_id
		   AND g.org_id = $1 AND c.name = $2 AND c.removed_at IS NULL
		   AND g.granted_at IS NOT NULL AND g.revoked_at IS NULL`,
		int64(org), name, int64(by))
	if err != nil {
		return fmt.Errorf("store: revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoGrant, name)
	}
	return nil
}

// DraftID identifies a draft.
type DraftID int64

// DraftRow is one row of a draft: what was read, and what it was read as.
type DraftRow struct {
	Line int `json:"line"`
	// Amount is exact, in stroops, as a string: JSON numbers are doubles.
	Amount      string `json:"amount"`
	Destination string `json:"destination"`
	Label       string `json:"label,omitempty"`
	Memo        string `json:"memo,omitempty"`
	// SaidAmount and SaidDestination are the cells as they were read.
	SaidAmount      string `json:"said_amount"`
	SaidDestination string `json:"said_destination"`
}

// Draft is a payroll read from a connector, awaiting a decision.
type Draft struct {
	ID        DraftID
	Org       ledger.OrgID
	Connector ConnectorID
	Selector  string
	Memo      string
	Rows      []DraftRow
	Total     money.Stroops
	Status    string
	Proposal  ProposalID
	// Exactly one of CreatedBy and AgentToken is set: a member who read it from
	// a connector, or an agent that wrote it. AgentLabel names the agent's
	// token, for whoever reviews the draft.
	CreatedBy  MemberID
	AgentToken int64
	AgentLabel string
	CreatedAt  time.Time
}

// SaveDraft records a draft.
func (s *Store) SaveDraft(ctx context.Context, d Draft) (Draft, error) {
	if len(d.Rows) == 0 || d.Total <= 0 {
		return Draft{}, errors.New("store: a draft has rows and a positive total")
	}
	encoded, err := json.Marshal(d.Rows)
	if err != nil {
		return Draft{}, fmt.Errorf("store: encode draft rows: %w", err)
	}
	if (d.CreatedBy == 0) == (d.AgentToken == 0) {
		return Draft{}, errors.New("store: a draft has exactly one author, a member or an agent")
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO drafts (org_id, connector_id, selector, memo, rows, row_count,
		                    total_stroops, created_by, agent_token_id)
		VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5, $6, $7, NULLIF($8::bigint, 0), NULLIF($9::bigint, 0))
		RETURNING id, status, created_at`,
		int64(d.Org), int64(d.Connector), d.Selector, d.Memo, encoded, len(d.Rows),
		int64(d.Total), int64(d.CreatedBy), d.AgentToken).Scan(&d.ID, &d.Status, &d.CreatedAt)
	if err != nil {
		return Draft{}, fmt.Errorf("store: save draft: %w", err)
	}
	return d, nil
}

const draftColumns = `d.id, d.org_id, COALESCE(d.connector_id, 0), d.selector, d.memo, d.rows,
	d.total_stroops, d.status, COALESCE(d.proposal_id, 0), COALESCE(d.created_by, 0),
	COALESCE(d.agent_token_id, 0), COALESCE(t.label, ''), d.created_at`

const draftFrom = `drafts d LEFT JOIN mcp_tokens t ON t.id = d.agent_token_id AND t.org_id = d.org_id`

func scanDraft(row pgx.Row) (Draft, error) {
	var (
		d       Draft
		encoded []byte
		total   int64
	)
	if err := row.Scan(&d.ID, &d.Org, &d.Connector, &d.Selector, &d.Memo, &encoded, &total,
		&d.Status, &d.Proposal, &d.CreatedBy, &d.AgentToken, &d.AgentLabel, &d.CreatedAt); err != nil {
		return Draft{}, err
	}
	d.Total = money.Stroops(total)
	if err := json.Unmarshal(encoded, &d.Rows); err != nil {
		return Draft{}, fmt.Errorf("store: decode draft rows: %w", err)
	}
	return d, nil
}

// OpenDraft returns a draft that is still awaiting a decision.
func (s *Store) OpenDraft(ctx context.Context, org ledger.OrgID, id DraftID) (Draft, error) {
	d, err := scanDraft(s.pool.QueryRow(ctx, `
		SELECT `+draftColumns+` FROM `+draftFrom+`
		 WHERE d.org_id = $1 AND d.id = $2 AND d.status = 'open'`, int64(org), int64(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, fmt.Errorf("%w: %d", ErrNoDraft, id)
	}
	if err != nil {
		return Draft{}, fmt.Errorf("store: draft %d: %w", id, err)
	}
	return d, nil
}

// Drafts lists a workspace's recent drafts, newest first.
func (s *Store) Drafts(ctx context.Context, org ledger.OrgID, limit int) ([]Draft, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+draftColumns+` FROM `+draftFrom+`
		 WHERE d.org_id = $1 ORDER BY d.created_at DESC, d.id DESC LIMIT $2`, int64(org), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list drafts: %w", err)
	}
	defer rows.Close()

	var out []Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan draft: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ResolveDraft closes an open draft: proposed, with the proposal it became, or
// discarded. Conditional on the draft still being open, so two people acting on
// one draft at the same moment cannot both propose it.
func (s *Store) ResolveDraft(
	ctx context.Context, org ledger.OrgID, id DraftID, status string, proposal ProposalID,
) error {
	var arg any
	switch status {
	case "proposed":
		if proposal == 0 {
			return errors.New("store: a proposed draft names its proposal")
		}
		arg = int64(proposal)
	case "discarded":
	default:
		return fmt.Errorf("store: %q does not close a draft", status)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE drafts SET status = $3, proposal_id = $4, resolved_at = now()
		 WHERE org_id = $1 AND id = $2 AND status = 'open'`,
		int64(org), int64(id), status, arg)
	if err != nil {
		return fmt.Errorf("store: resolve draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrNoDraft, id)
	}
	return nil
}

// AllowConnectorCall is the shared rate limit: it counts calls to one of an
// org's connectors over the last hour, across every instance of stelfin, and
// records this one if there is room.
//
// Serialised per connector with a transaction-scoped advisory lock, so two
// instances asking at the same moment cannot both take the last slot. Rows
// older than the window are pruned under the same lock, so the table holds an
// hour of calls and no more.
func (s *Store) AllowConnectorCall(
	ctx context.Context, org ledger.OrgID, connector string, perHour int, now time.Time,
) (bool, error) {
	if perHour <= 0 {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		int32(int64(org)), connector); err != nil {
		return false, fmt.Errorf("store: lock connector rate: %w", err)
	}
	since := now.Add(-time.Hour)
	if _, err := tx.Exec(ctx, `
		DELETE FROM connector_calls WHERE org_id = $1 AND connector = $2 AND called_at <= $3`,
		int64(org), connector, since); err != nil {
		return false, fmt.Errorf("store: prune connector calls: %w", err)
	}
	var used int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM connector_calls WHERE org_id = $1 AND connector = $2`,
		int64(org), connector).Scan(&used); err != nil {
		return false, fmt.Errorf("store: count connector calls: %w", err)
	}
	if used >= perHour {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO connector_calls (org_id, connector, called_at) VALUES ($1, $2, $3)`,
		int64(org), connector, now); err != nil {
		return false, fmt.Errorf("store: record connector call: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit connector call: %w", err)
	}
	return true, nil
}
