package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger"
)

// Refusals from the enrolment limiter.
//
// Distinct errors because the answers are genuinely different and the person
// asking can act on some of them: "your workspace has not turned this on" is
// something an admin fixes, "you already have one" is not a problem at all, and
// "the operator is out of float" is nobody's fault.
var (
	// ErrAlreadyGranted reports a member who has already had an account paid
	// for. One per member, ever.
	ErrAlreadyGranted = errors.New("store: this member already has a provisioned account")

	// ErrProvisioningOff reports an org that has not enabled provisioning.
	ErrProvisioningOff = errors.New("store: provisioning is not enabled for this workspace")

	// ErrDailyCapReached reports an org at its daily limit.
	ErrDailyCapReached = errors.New("store: this workspace has provisioned its daily limit")

	// ErrGlobalCapReached reports the deployment at its own limit.
	ErrGlobalCapReached = errors.New("store: too many accounts provisioned recently")

	// ErrFloatTooLow reports a sponsor account with too little XLM left to keep
	// provisioning safely.
	ErrFloatTooLow = errors.New("store: the operator's XLM float is too low")

	// ErrTooNew reports an identity or a space that has not existed long
	// enough. A chat account created seconds ago asking for a sponsored wallet
	// is the shape of a script, not a person.
	ErrTooNew = errors.New("store: too new to be provisioned an account")
)

// Enrolment limits that are properties of the deployment rather than of any
// tenant.
const (
	// GlobalDailyGrants caps how many accounts the whole deployment will
	// sponsor in a day, whatever any org's own cap says.
	GlobalDailyGrants = 200

	// MinIdentityAge is how long a chat account must have been known before it
	// can be given a sponsored wallet. Cheap, and it defeats the trivial
	// script: create account, add to group, ask for money.
	MinIdentityAge = 10 * time.Minute

	// MinSpaceAge applies the same idea to a workspace with no verified
	// treasury behind it.
	MinSpaceAge = time.Hour

	// FloatFloorMultiple is how many further grants the sponsor account must be
	// able to cover before another is allowed.
	//
	// A floor rather than a count, because it degrades in the right direction:
	// provisioning slows as the float drains instead of stopping dead at an
	// arbitrary number, and it cannot be tuned into a state where the operator
	// promises reserves it cannot pay.
	FloatFloorMultiple = 50
)

// GrantParams describes an account the operator is about to pay for.
type GrantParams struct {
	Org      ledger.OrgID
	Member   MemberID
	Identity IdentityID
	Address  string
	// ReserveCost is what this account will lock: the base reserve plus one per
	// trustline. Recorded rather than recomputed later, so a reclaim knows what
	// it is releasing.
	ReserveCost money.Stroops
	// SponsorBalance is the sponsor account's current XLM, read from the chain
	// by the caller. Zero means unknown, which is treated as too low.
	SponsorBalance money.Stroops
}

// GrantEnrollment records that the operator is paying for an account, after
// checking every limit.
//
// All the checks and the insert happen in one transaction, so two requests
// arriving together cannot both pass a count that only one of them should.
// Getting that wrong does not produce a wrong number in a report — it produces
// XLM the operator has spent and cannot get back.
//
// The order runs cheapest and most specific first, so the error someone sees is
// the most useful one available.
func (s *Store) GrantEnrollment(ctx context.Context, p GrantParams) error {
	if p.ReserveCost <= 0 {
		return errors.New("store: a grant must record what it costs")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. One per member, ever. A hard cap, not a rate limit: nobody needs the
	//    operator to pay for a second account for them.
	var existing bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM enrollment_grants WHERE member_id = $1`, int64(p.Member),
	).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: member %d", ErrAlreadyGranted, p.Member)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: check existing grant: %w", err)
	}

	// 2. The workspace has to have turned this on — and it cannot, until it has
	//    proved control of a treasury. That is the real anti-abuse gate: a
	//    drive-by install costs nothing, because a stranger who adds the bot
	//    somewhere has no treasury to prove and therefore no budget to spend.
	var budget, dailyCap int64
	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT enroll_budget_stroops, enroll_daily_cap, created_at FROM orgs WHERE id = $1`,
		int64(p.Org),
	).Scan(&budget, &dailyCap, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrOrgNotFound, p.Org)
	}
	if err != nil {
		return fmt.Errorf("store: read org policy: %w", err)
	}
	if budget <= 0 {
		return ErrProvisioningOff
	}
	// Asked of the database, in this transaction, rather than taken as a
	// parameter. A boolean the caller supplies is a boolean somebody
	// eventually passes true — and the whole point of this gate is that it
	// cannot be satisfied by anything other than a signature that reached a
	// treasury's medium threshold.
	var verified bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM org_treasuries WHERE org_id = $1)`,
		int64(p.Org)).Scan(&verified); err != nil {
		return fmt.Errorf("store: check verified treasury: %w", err)
	}
	if !verified {
		return fmt.Errorf("%w: no treasury has been verified for this workspace", ErrProvisioningOff)
	}

	// 3. Per-org rate.
	var todayOrg int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM enrollment_grants
		 WHERE org_id = $1 AND granted_at > now() - interval '24 hours'`,
		int64(p.Org),
	).Scan(&todayOrg); err != nil {
		return fmt.Errorf("store: count org grants: %w", err)
	}
	if todayOrg >= dailyCap {
		return fmt.Errorf("%w: %d in the last day", ErrDailyCapReached, todayOrg)
	}

	// 4. Deployment-wide rate, and the float floor. The floor matters more: a
	//    count can be tuned past what the operator can afford, a balance cannot.
	var todayAll int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM enrollment_grants WHERE granted_at > now() - interval '24 hours'`,
	).Scan(&todayAll); err != nil {
		return fmt.Errorf("store: count all grants: %w", err)
	}
	if todayAll >= GlobalDailyGrants {
		return fmt.Errorf("%w: %d in the last day", ErrGlobalCapReached, todayAll)
	}
	if p.SponsorBalance < p.ReserveCost*FloatFloorMultiple {
		return fmt.Errorf("%w: %s left, want at least %s",
			ErrFloatTooLow, p.SponsorBalance, p.ReserveCost*FloatFloorMultiple)
	}

	// 5. Identity age.
	var identityAge time.Time
	if err := tx.QueryRow(ctx,
		`SELECT linked_at FROM user_identities WHERE id = $1 AND org_id = $2`,
		int64(p.Identity), int64(p.Org),
	).Scan(&identityAge); err != nil {
		return fmt.Errorf("store: read identity age: %w", err)
	}
	if time.Since(identityAge) < MinIdentityAge {
		return fmt.Errorf("%w: this account was first seen %s ago",
			ErrTooNew, time.Since(identityAge).Truncate(time.Second))
	}

	// 6. Space age, as a second line behind the treasury gate.
	if time.Since(createdAt) < MinSpaceAge {
		return fmt.Errorf("%w: this workspace was created %s ago",
			ErrTooNew, time.Since(createdAt).Truncate(time.Second))
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO enrollment_grants (org_id, identity_id, member_id, address, reserve_cost)
		VALUES ($1, $2, $3, $4, $5)`,
		int64(p.Org), int64(p.Identity), int64(p.Member), p.Address, int64(p.ReserveCost),
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: member %d", ErrAlreadyGranted, p.Member)
		}
		return fmt.Errorf("store: record grant: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Grant is a recorded sponsorship.
type Grant struct {
	Org         ledger.OrgID
	Member      MemberID
	Address     string
	ReserveCost money.Stroops
	GrantedAt   time.Time
	Reclaimed   bool
}

// GrantFor returns the sponsorship recorded against an address, if there is one.
func (s *Store) GrantFor(ctx context.Context, org ledger.OrgID, address string) (Grant, bool, error) {
	var g Grant
	var reclaimed *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT org_id, member_id, address, reserve_cost, granted_at, reclaimed_at
		  FROM enrollment_grants WHERE org_id = $1 AND address = $2`,
		int64(org), address,
	).Scan(&g.Org, &g.Member, &g.Address, &g.ReserveCost, &g.GrantedAt, &reclaimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, fmt.Errorf("store: grant for %s: %w", address, err)
	}
	g.Reclaimed = reclaimed != nil
	return g, true, nil
}

// ReclaimGrant marks a sponsorship as released, once the reserve is actually
// back.
//
// Called after the merge lands, not before: a grant marked reclaimed while the
// reserve is still locked would let the same member be provisioned again at the
// operator's expense.
func (s *Store) ReclaimGrant(ctx context.Context, org ledger.OrgID, address string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE enrollment_grants SET reclaimed_at = now()
		 WHERE org_id = $1 AND address = $2 AND reclaimed_at IS NULL`,
		int64(org), address)
	if err != nil {
		return fmt.Errorf("store: reclaim grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: no unreclaimed grant for %s", address)
	}
	return nil
}

// SetEnrollmentPolicy turns provisioning on or off for an org.
func (s *Store) SetEnrollmentPolicy(
	ctx context.Context, org ledger.OrgID, budget money.Stroops, dailyCap int,
) error {
	if budget < 0 || dailyCap < 0 {
		return errors.New("store: enrolment policy cannot be negative")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE orgs SET enroll_budget_stroops = $2, enroll_daily_cap = $3 WHERE id = $1`,
		int64(org), int64(budget), dailyCap)
	if err != nil {
		return fmt.Errorf("store: set enrolment policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrOrgNotFound, org)
	}
	return nil
}
