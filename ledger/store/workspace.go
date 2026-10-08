package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

var (
	// ErrNoSpaceCode reports a space code that is unknown, expired or spent.
	// One error for all three, as for every other code here: which it was
	// narrows a guess.
	ErrNoSpaceCode = errors.New("store: no live space code")

	// ErrBeneficiaryExists reports a label this member has already saved.
	ErrBeneficiaryExists = errors.New("store: a recipient is already saved under that name")

	// ErrNoBeneficiary reports a label this member has not saved.
	ErrNoBeneficiary = errors.New("store: no recipient is saved under that name")
)

// IssueSpaceCode mints a code that attaches another chat space to org.
//
// Any earlier live code is replaced. The code uses the same alphabet as a link
// code — it is read off one screen and typed into another — but is longer,
// because what it grants is larger: a link code adds a handle to one member,
// and this adds a room full of people to a workspace.
func (s *Store) IssueSpaceCode(
	ctx context.Context, org ledger.OrgID, by MemberID, ttl time.Duration,
) (string, error) {
	var b strings.Builder
	for i := 0; i < 2; i++ {
		part, err := newLinkCode()
		if err != nil {
			return "", err
		}
		b.WriteString(part)
	}
	code := b.String()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM space_link_codes WHERE org_id = $1 AND consumed_at IS NULL`,
		int64(org)); err != nil {
		return "", fmt.Errorf("store: supersede space code: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO space_link_codes (code_hash, org_id, issued_by, expires_at)
		VALUES ($1, $2, $3, now() + $4::interval)`,
		hashSpaceCode(code), int64(org), int64(by), ttl.String()); err != nil {
		return "", fmt.Errorf("store: issue space code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit: %w", err)
	}
	return code, nil
}

// ClaimSpaceCode spends a code, attaching a space to the org that issued it.
//
// The code is claimed and the space attached in one transaction. A code spent
// on a space that then failed to attach would be authority used up for nothing;
// a space attached under a code that another caller is also spending would be
// one code admitting two rooms.
func (s *Store) ClaimSpaceCode(
	ctx context.Context, code string, channel chat.Channel, spaceID, by string,
) (Org, error) {
	if !channel.Valid() {
		return Org{}, fmt.Errorf("store: unknown channel %q", channel)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Org{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var org int64
	err = tx.QueryRow(ctx, `
		UPDATE space_link_codes
		   SET consumed_at = now()
		 WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING org_id`, hashSpaceCode(code)).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrNoSpaceCode
	}
	if err != nil {
		return Org{}, fmt.Errorf("store: claim space code: %w", err)
	}

	var attached bool
	err = tx.QueryRow(ctx, `
		INSERT INTO org_spaces (org_id, channel, space_id, installed_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (channel, space_id) DO NOTHING
		RETURNING true`, org, string(channel), spaceID, by).Scan(&attached)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, fmt.Errorf("%w: %s/%s", ErrSpaceRegistered, channel, spaceID)
	}
	if err != nil {
		return Org{}, fmt.Errorf("store: attach space: %w", err)
	}

	out, err := scanOrg(tx.QueryRow(ctx,
		`SELECT `+orgColumns+` FROM orgs WHERE id = $1`, org))
	if err != nil {
		return Org{}, fmt.Errorf("store: read org %d: %w", org, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Org{}, fmt.Errorf("store: commit: %w", err)
	}
	return out, nil
}

// hashSpaceCode normalises and hashes a code. Case and surrounding space are
// forgiven because a person typed it; nothing else is.
func hashSpaceCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

// Spaces lists the chat spaces attached to an org.
func (s *Store) Spaces(ctx context.Context, org ledger.OrgID) ([]Space, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel, space_id, active FROM org_spaces
		 WHERE org_id = $1 ORDER BY created_at, channel, space_id`, int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: list spaces: %w", err)
	}
	defer rows.Close()

	var out []Space
	for rows.Next() {
		var sp Space
		var channel string
		if err := rows.Scan(&channel, &sp.SpaceID, &sp.Active); err != nil {
			return nil, fmt.Errorf("store: scan space: %w", err)
		}
		sp.Channel = chat.Channel(channel)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// Space is one chat space attached to an org.
type Space struct {
	Channel chat.Channel
	SpaceID string
	Active  bool
}

// Beneficiary is a recipient a member has saved under a name.
type Beneficiary struct {
	Label   string
	Address string
}

// SaveBeneficiary records a recipient under a name, for one member.
//
// Saved per member and not per workspace, on purpose. "Send 50 to mum" has to
// mean the speaker's mother; a shared address book would let one member decide
// where another member's sentence goes.
func (s *Store) SaveBeneficiary(
	ctx context.Context, org ledger.OrgID, ownerRef, label, address string,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO beneficiaries (org_id, owner_ref, label, address)
		VALUES ($1, $2, $3, $4)`, int64(org), ownerRef, label, address)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrBeneficiaryExists, label)
	}
	if err != nil {
		return fmt.Errorf("store: save recipient: %w", err)
	}
	return nil
}

// Beneficiaries lists what a member has saved, by name.
func (s *Store) Beneficiaries(ctx context.Context, org ledger.OrgID, ownerRef string) ([]Beneficiary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT label, address FROM beneficiaries
		 WHERE org_id = $1 AND owner_ref = $2 ORDER BY lower(label)`, int64(org), ownerRef)
	if err != nil {
		return nil, fmt.Errorf("store: list recipients: %w", err)
	}
	defer rows.Close()

	var out []Beneficiary
	for rows.Next() {
		var b Beneficiary
		if err := rows.Scan(&b.Label, &b.Address); err != nil {
			return nil, fmt.Errorf("store: scan recipient: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ForgetBeneficiary removes a saved recipient.
func (s *Store) ForgetBeneficiary(ctx context.Context, org ledger.OrgID, ownerRef, label string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM beneficiaries
		 WHERE org_id = $1 AND owner_ref = $2 AND lower(label) = lower($3)`,
		int64(org), ownerRef, label)
	if err != nil {
		return fmt.Errorf("store: forget recipient: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoBeneficiary, label)
	}
	return nil
}

// SpendPolicy is the ceilings an org has set for itself. Zero means none.
type SpendPolicy struct {
	// PerPayment bounds what one proposal may move.
	PerPayment money.Stroops
	// Daily bounds what may be proposed in any twenty-four hours.
	Daily money.Stroops
}

// SpendPolicyOf reads an org's ceilings.
func (s *Store) SpendPolicyOf(ctx context.Context, org ledger.OrgID) (SpendPolicy, error) {
	var per, daily int64
	err := s.pool.QueryRow(ctx, `
		SELECT payment_ceiling_stroops, daily_ceiling_stroops FROM orgs WHERE id = $1`,
		int64(org)).Scan(&per, &daily)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpendPolicy{}, fmt.Errorf("%w: %d", ErrOrgNotFound, org)
	}
	if err != nil {
		return SpendPolicy{}, fmt.Errorf("store: read spend policy: %w", err)
	}
	return SpendPolicy{PerPayment: money.Stroops(per), Daily: money.Stroops(daily)}, nil
}

// SetSpendPolicy replaces an org's ceilings.
func (s *Store) SetSpendPolicy(ctx context.Context, org ledger.OrgID, p SpendPolicy) error {
	if p.PerPayment < 0 || p.Daily < 0 {
		return errors.New("store: a ceiling cannot be negative")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE orgs SET payment_ceiling_stroops = $2, daily_ceiling_stroops = $3 WHERE id = $1`,
		int64(org), int64(p.PerPayment), int64(p.Daily))
	if err != nil {
		return fmt.Errorf("store: set spend policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrOrgNotFound, org)
	}
	return nil
}

// ProposedSince sums what an org's proposals have moved, or would move, since
// a moment.
//
// Open and executed proposals both count, and nothing else does. An open one is
// money the workspace has already asked its signers for; counting only what
// executed would let a day's ceiling be queued up many times over and then
// signed at leisure. Cancelled, failed and expired proposals moved nothing.
//
// Passing zero for org sums across every org, which is what the deployment's
// own ceiling is checked against.
func (s *Store) ProposedSince(ctx context.Context, org ledger.OrgID, since time.Time) (money.Stroops, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_stroops), 0)::bigint FROM proposals
		 WHERE ($1 = 0 OR org_id = $1)
		   AND created_at > $2
		   AND status IN ('open', 'executed')`, int64(org), since).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: sum proposals: %w", err)
	}
	return money.Stroops(total), nil
}

// SetOrgStatus suspends or reactivates an org.
//
// The kill switch for one tenant. It is deliberately not reachable from chat:
// the people who can suspend a workspace are the people operating the
// deployment, and a command would make it something a compromised admin
// account in one workspace could do to itself at the worst moment, or be
// talked into.
func (s *Store) SetOrgStatus(ctx context.Context, org ledger.OrgID, status string) error {
	if status != OrgActive && status != OrgSuspended {
		return fmt.Errorf("store: %q is not an org status", status)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE orgs SET status = $2 WHERE id = $1 AND kind = 'dao'`, int64(org), status)
	if err != nil {
		return fmt.Errorf("store: set org status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrOrgNotFound, org)
	}
	return nil
}

// Orgs lists every DAO org, oldest first, for the operator's own tooling.
func (s *Store) Orgs(ctx context.Context) ([]Org, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+orgColumns+` FROM orgs WHERE kind = 'dao' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list orgs: %w", err)
	}
	defer rows.Close()

	var out []Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan org: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrgsOffNetwork counts orgs whose books were written for a different network.
//
// An org is bound to one network for its lifetime. A deployment pointed at the
// other one would read every treasury address it holds as an account on a chain
// it was never proved against — so the count has to be zero before anything
// serves a request.
func (s *Store) OrgsOffNetwork(ctx context.Context, network string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM orgs WHERE network <> $1`, network).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count orgs off network: %w", err)
	}
	return n, nil
}
