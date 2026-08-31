package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

// Proposal statuses.
const (
	ProposalOpen      = "open"
	ProposalExecuted  = "executed"
	ProposalFailed    = "failed"
	ProposalCancelled = "cancelled"
	ProposalExpired   = "expired"
)

// Proposal event kinds.
const (
	EventCreated    = "created"
	EventSigned     = "signed"
	EventSubmitted  = "submitted"
	EventExecuted   = "executed"
	EventFailed     = "failed"
	EventCancelled  = "cancelled"
	EventExpired    = "expired"
	EventSuperseded = "superseded"
)

var (
	// ErrNoProposal reports a proposal that does not exist in this org.
	ErrNoProposal = errors.New("store: no such proposal")

	// ErrProposalAlreadyOpen reports a treasury that already has one.
	//
	// Not a queue-discipline preference. Two open proposals from one account
	// both reserve the same next sequence number, so whichever executes first
	// silently kills the other — refusing the second turns that into something
	// a member can act on.
	ErrProposalAlreadyOpen = errors.New("store: this treasury already has an open proposal")

	// ErrProposalClosed reports an approval arriving after the fact.
	ErrProposalClosed = errors.New("store: this proposal is no longer open")
)

// ProposalID identifies a proposal.
type ProposalID int64

// Proposal is an envelope waiting for enough signatures.
type Proposal struct {
	ID       ProposalID
	Org      ledger.OrgID
	Treasury TreasuryID
	Kind     string

	// XDR is the unsigned envelope every approver sees, and Hash is what each
	// signature covers.
	XDR  string
	Hash string

	// SourceSeq is the sequence number this envelope reserves. Kept so the
	// submit path can say "the treasury moved on" instead of surfacing a
	// tx_bad_seq nobody can connect to anything.
	SourceSeq int64

	Description string
	Digest      []byte

	Status      string
	CreatedBy   MemberID
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ResolvedAt  *time.Time
	SubmittedTx string
}

// CreateProposalParams describes a new proposal.
type CreateProposalParams struct {
	Org      ledger.OrgID
	Treasury TreasuryID
	Kind     string

	XDR       string
	Hash      string
	SourceSeq int64

	// Description is the canonical text every approver was shown, and Digest
	// its SHA-256. The digest is what an on-chain proposal carries as its memo,
	// so the document read in chat and the one voted on are provably the same.
	Description string
	Digest      []byte

	CreatedBy MemberID
	ExpiresAt time.Time
}

const proposalColumns = `
	id, org_id, treasury_id, kind, base_xdr, tx_hash, source_seq,
	description_canonical, description_digest, status, created_by,
	created_at, expires_at, resolved_at, COALESCE(submitted_hash, '')`

func scanProposal(row pgx.Row) (Proposal, error) {
	var p Proposal
	err := row.Scan(&p.ID, &p.Org, &p.Treasury, &p.Kind, &p.XDR, &p.Hash, &p.SourceSeq,
		&p.Description, &p.Digest, &p.Status, &p.CreatedBy,
		&p.CreatedAt, &p.ExpiresAt, &p.ResolvedAt, &p.SubmittedTx)
	return p, err
}

// CreateProposal opens a proposal and records that it was created.
//
// Both in one transaction: a proposal with no 'created' event would leave the
// audit trail starting at whoever signed first, which is exactly the person it
// should not start at.
func (s *Store) CreateProposal(ctx context.Context, p CreateProposalParams) (Proposal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	out, err := scanProposal(tx.QueryRow(ctx, `
		INSERT INTO proposals (
			org_id, treasury_id, kind, base_xdr, tx_hash, source_seq,
			description_canonical, description_digest, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+proposalColumns,
		int64(p.Org), int64(p.Treasury), p.Kind, p.XDR, p.Hash, p.SourceSeq,
		p.Description, p.Digest, int64(p.CreatedBy), p.ExpiresAt,
	))
	if isUniqueViolation(err) {
		// Either this treasury already has an open proposal, or this exact
		// envelope has been proposed before. Both are the same instruction to
		// the caller — look at what is already open — and distinguishing them
		// here would mean parsing a constraint name.
		return Proposal{}, fmt.Errorf("%w: %d", ErrProposalAlreadyOpen, p.Treasury)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("store: create proposal: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO proposal_events (proposal_id, kind, detail)
		VALUES ($1, $2, $3)`, int64(out.ID), EventCreated, p.Kind); err != nil {
		return Proposal{}, fmt.Errorf("store: record creation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Proposal{}, fmt.Errorf("store: commit proposal: %w", err)
	}
	return out, nil
}

// Proposal returns one proposal.
func (s *Store) Proposal(ctx context.Context, org ledger.OrgID, id ProposalID) (Proposal, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx,
		`SELECT `+proposalColumns+` FROM proposals WHERE id = $1 AND org_id = $2`,
		int64(id), int64(org)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: %d", ErrNoProposal, id)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("store: proposal %d: %w", id, err)
	}
	return p, nil
}

// OpenProposal returns the treasury's open proposal, if it has one.
func (s *Store) OpenProposal(
	ctx context.Context, org ledger.OrgID, treasury TreasuryID,
) (Proposal, bool, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx,
		`SELECT `+proposalColumns+`
		   FROM proposals
		  WHERE treasury_id = $1 AND org_id = $2 AND status = 'open'`,
		int64(treasury), int64(org)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, fmt.Errorf("store: open proposal: %w", err)
	}
	return p, true, nil
}

// Proposals lists an org's proposals, newest first.
func (s *Store) Proposals(ctx context.Context, org ledger.OrgID, limit int) ([]Proposal, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+proposalColumns+`
		   FROM proposals WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		int64(org), limit)
	if err != nil {
		return nil, fmt.Errorf("store: proposals: %w", err)
	}
	defer rows.Close()

	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProposalSignature is one approval.
type ProposalSignature struct {
	Signer    string
	Signature []byte
	Weight    int32
	AddedBy   IdentityID
	CreatedAt time.Time
}

// AddSignature records an approval against a proposal.
//
// Idempotent per signer. The same key signing twice is not more approval, and a
// member who taps approve twice on a slow connection has not done anything
// wrong — so the second write is accepted and changes nothing rather than
// erroring at them.
func (s *Store) AddSignature(
	ctx context.Context, org ledger.OrgID, id ProposalID, sig ProposalSignature,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The status is read FOR UPDATE inside the same transaction as the insert.
	// Checking it beforehand would leave the window where a proposal is
	// executed between the check and the write, and an approval landing on an
	// already-submitted envelope is an approval nobody can withdraw.
	var status string
	var expires time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, expires_at FROM proposals
		 WHERE id = $1 AND org_id = $2 FOR UPDATE`,
		int64(id), int64(org)).Scan(&status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNoProposal, id)
	}
	if err != nil {
		return fmt.Errorf("store: lock proposal %d: %w", id, err)
	}
	if status != ProposalOpen {
		return fmt.Errorf("%w: %d is %s", ErrProposalClosed, id, status)
	}
	if !expires.After(time.Now()) {
		return fmt.Errorf("%w: %d expired at %s", ErrProposalClosed, id, expires.Format(time.RFC3339))
	}

	var addedBy any
	if sig.AddedBy != 0 {
		addedBy = int64(sig.AddedBy)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO proposal_signatures (
			proposal_id, signer, signature, weight_at_signing, added_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (proposal_id, signer) DO NOTHING`,
		int64(id), sig.Signer, sig.Signature, sig.Weight, addedBy)
	if err != nil {
		return fmt.Errorf("store: add signature: %w", err)
	}

	// Only a signature that was actually new is worth an event. A double tap
	// should not produce two lines in the audit trail saying two people
	// approved.
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO proposal_events (proposal_id, kind, actor, detail)
			VALUES ($1, $2, $3, $4)`,
			int64(id), EventSigned, addedBy, sig.Signer); err != nil {
			return fmt.Errorf("store: record signature: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// Signatures returns a proposal's approvals in the order they arrived.
func (s *Store) Signatures(
	ctx context.Context, org ledger.OrgID, id ProposalID,
) ([]ProposalSignature, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ps.signer, ps.signature, ps.weight_at_signing,
		       COALESCE(ps.added_by, 0), ps.created_at
		  FROM proposal_signatures ps
		  JOIN proposals p ON p.id = ps.proposal_id
		 WHERE ps.proposal_id = $1 AND p.org_id = $2
		 ORDER BY ps.created_at, ps.signer`,
		int64(id), int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: signatures: %w", err)
	}
	defer rows.Close()

	var out []ProposalSignature
	for rows.Next() {
		var sig ProposalSignature
		if err := rows.Scan(&sig.Signer, &sig.Signature, &sig.Weight,
			&sig.AddedBy, &sig.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan signature: %w", err)
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// ResolveProposal closes a proposal and records why.
//
// submittedHash is the hash of what actually reached the network, which differs
// from the proposal's own hash whenever the envelope was fee-bumped — as it
// usually is, since the operator pays.
func (s *Store) ResolveProposal(
	ctx context.Context, org ledger.OrgID, id ProposalID,
	status, submittedHash, detail string, actor IdentityID,
) error {
	if status == ProposalOpen {
		return errors.New("store: resolving a proposal to 'open' is not a resolution")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var hash any
	if submittedHash != "" {
		hash = submittedHash
	}
	var actorID any
	if actor != 0 {
		actorID = int64(actor)
	}

	// status = 'open' in the predicate makes this a compare-and-set: two
	// members racing to execute produce one resolution and one plain "no longer
	// open", rather than a second write that overwrites the first outcome.
	var found ProposalID
	err = tx.QueryRow(ctx, `
		UPDATE proposals
		   SET status = $3, resolved_at = now(),
		       submitted_hash = COALESCE($4, submitted_hash)
		 WHERE id = $1 AND org_id = $2 AND status = 'open'
		RETURNING id`,
		int64(id), int64(org), status, hash).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrProposalClosed, id)
	}
	if err != nil {
		return fmt.Errorf("store: resolve proposal %d: %w", id, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO proposal_events (proposal_id, kind, actor, detail)
		VALUES ($1, $2, $3, $4)`, int64(id), status, actorID, detail); err != nil {
		return fmt.Errorf("store: record resolution: %w", err)
	}
	return tx.Commit(ctx)
}

// ProposalEvent is one line of a proposal's history.
type ProposalEvent struct {
	Kind      string
	Actor     IdentityID
	Detail    string
	CreatedAt time.Time
}

// ProposalHistory returns what happened to a proposal, in order.
func (s *Store) ProposalHistory(
	ctx context.Context, org ledger.OrgID, id ProposalID,
) ([]ProposalEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.kind, COALESCE(e.actor, 0), e.detail, e.created_at
		  FROM proposal_events e
		  JOIN proposals p ON p.id = e.proposal_id
		 WHERE e.proposal_id = $1 AND p.org_id = $2
		 ORDER BY e.id`,
		int64(id), int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: proposal history: %w", err)
	}
	defer rows.Close()

	var out []ProposalEvent
	for rows.Next() {
		var e ProposalEvent
		if err := rows.Scan(&e.Kind, &e.Actor, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExpireProposals closes proposals whose window has passed.
//
// Run on a timer rather than checked lazily on read, because an expired
// proposal still holds the treasury's one open slot. Left to a lazy check,
// nobody could open a new proposal until somebody happened to look at the old
// one — and the person blocked is the person who does not know it exists.
func (s *Store) ExpireProposals(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH expired AS (
			UPDATE proposals SET status = 'expired', resolved_at = now()
			 WHERE status = 'open' AND expires_at <= now()
			RETURNING id
		)
		INSERT INTO proposal_events (proposal_id, kind, detail)
		SELECT id, 'expired', 'the signing window closed' FROM expired`)
	if err != nil {
		return 0, fmt.Errorf("store: expire proposals: %w", err)
	}
	return tag.RowsAffected(), nil
}
