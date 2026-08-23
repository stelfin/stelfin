package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger"
)

var (
	// ErrMemberNotFound reports a member that does not exist in this org.
	ErrMemberNotFound = errors.New("store: member not found")
	// ErrAddressTaken reports an address already bound to another member of the
	// same org. Two members sharing one address makes "who approved this"
	// unanswerable.
	ErrAddressTaken = errors.New("store: address already belongs to another member")
)

// MemberID identifies a member within an org.
type MemberID int64

// How a member came by their address.
const (
	AddressLinked      = "linked"
	AddressProvisioned = "provisioned"
)

// Member is one person in one org.
type Member struct {
	ID          MemberID
	Org         ledger.OrgID
	DisplayName string
	// Address is empty until the member has proved they control one by
	// signature. A member exists as soon as they speak; authority over money
	// arrives separately and later.
	Address       string
	AddressSource string
	VerifiedAt    time.Time
	Status        string
}

// HasAddress reports whether this member has proved an address.
func (m Member) HasAddress() bool { return m.Address != "" }

// EnsureMember returns the member a chat identity speaks for, creating both if
// this is the first time they have been seen.
//
// The identity is what is looked up, and it is keyed on the platform's own
// immutable user id — never the handle, which can be renamed and, once
// released, taken by someone else.
func (s *Store) EnsureMember(
	ctx context.Context, org ledger.OrgID, channel chat.Channel, channelUserID, handle string,
) (Member, error) {
	if !channel.Valid() {
		return Member{}, fmt.Errorf("store: unknown channel %q", channel)
	}
	if channelUserID == "" {
		return Member{}, errors.New("store: channel user id is required")
	}

	if m, ok, err := s.MemberByIdentity(ctx, org, channel, channelUserID); err != nil || ok {
		if err == nil && m.DisplayName != handle && handle != "" {
			// The handle is display only, and keeping it current costs one
			// cheap write. Nothing is ever looked up by it.
			_, _ = s.pool.Exec(ctx,
				`UPDATE user_identities SET handle = $1
				  WHERE org_id = $2 AND channel = $3 AND channel_user_id = $4`,
				handle, int64(org), string(channel), channelUserID)
		}
		return m, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Member{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id MemberID
	if err := tx.QueryRow(ctx, `
		INSERT INTO members (org_id, display_name) VALUES ($1, $2) RETURNING id`,
		int64(org), handle,
	).Scan(&id); err != nil {
		return Member{}, fmt.Errorf("store: create member: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identities (member_id, org_id, channel, channel_user_id, handle)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, channel, channel_user_id) DO NOTHING`,
		int64(id), int64(org), string(channel), channelUserID, handle,
	); err != nil {
		return Member{}, fmt.Errorf("store: link identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Member{}, fmt.Errorf("store: commit: %w", err)
	}

	// Re-read rather than construct: two concurrent first messages from the
	// same person both insert a member, and the conflict on the identity index
	// means one of those members has nothing pointing at it. Reading back
	// returns whichever one won.
	m, ok, err := s.MemberByIdentity(ctx, org, channel, channelUserID)
	if err != nil {
		return Member{}, err
	}
	if !ok {
		return Member{}, fmt.Errorf("%w: %s/%s", ErrMemberNotFound, channel, channelUserID)
	}
	return m, nil
}

// memberColumns is qualified with the alias every query here uses, because one
// of them joins user_identities, which also has an id and an org_id.
const memberColumns = `m.id, m.org_id, m.display_name, COALESCE(m.address, ''),
	COALESCE(m.address_source, ''), COALESCE(m.address_verified_at, 'epoch'::timestamptz), m.status`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.Org, &m.DisplayName, &m.Address,
		&m.AddressSource, &m.VerifiedAt, &m.Status)
	return m, err
}

// MemberByIdentity resolves a chat identity to the member it speaks for.
func (s *Store) MemberByIdentity(
	ctx context.Context, org ledger.OrgID, channel chat.Channel, channelUserID string,
) (Member, bool, error) {
	m, err := scanMember(s.pool.QueryRow(ctx, `
		SELECT `+memberColumns+`
		  FROM members m
		  JOIN user_identities i ON i.member_id = m.id AND i.org_id = m.org_id
		 WHERE m.org_id = $1 AND i.channel = $2 AND i.channel_user_id = $3`,
		int64(org), string(channel), channelUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, false, nil
	}
	if err != nil {
		return Member{}, false, fmt.Errorf("store: member for %s/%s: %w", channel, channelUserID, err)
	}
	return m, true, nil
}

// Member returns one member by id.
func (s *Store) Member(ctx context.Context, org ledger.OrgID, id MemberID) (Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT `+memberColumns+` FROM members m WHERE m.org_id = $1 AND m.id = $2`,
		int64(org), int64(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, fmt.Errorf("%w: %d", ErrMemberNotFound, id)
	}
	if err != nil {
		return Member{}, fmt.Errorf("store: member %d: %w", id, err)
	}
	return m, nil
}

// SetMemberAddress records the address a member has proved they control.
//
// source says how: 'linked' for one they already had, 'provisioned' for one the
// operator paid the reserve to create. The verification timestamp is written in
// the same statement as the address, and the schema requires both or neither —
// an address without the proof that produced it is indistinguishable from a
// verified one, and would be trusted like one.
func (s *Store) SetMemberAddress(
	ctx context.Context, org ledger.OrgID, id MemberID, address, source string,
) error {
	if source != AddressLinked && source != AddressProvisioned {
		return fmt.Errorf("store: unknown address source %q", source)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE members
		   SET address = $3, address_source = $4, address_verified_at = now()
		 WHERE org_id = $1 AND id = $2`,
		int64(org), int64(id), address, source)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrAddressTaken, address)
	}
	if err != nil {
		return fmt.Errorf("store: set member address: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrMemberNotFound, id)
	}
	return nil
}

// LinkIdentity attaches another chat account to an existing member, so the same
// human on Telegram and Discord is one member with one wallet.
func (s *Store) LinkIdentity(
	ctx context.Context, org ledger.OrgID, id MemberID,
	channel chat.Channel, channelUserID, handle string, by MemberID,
) error {
	if !channel.Valid() {
		return fmt.Errorf("store: unknown channel %q", channel)
	}
	var linkedBy *int64
	if by != 0 {
		v := int64(by)
		linkedBy = &v
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_identities (member_id, org_id, channel, channel_user_id, handle, linked_by)
		VALUES ($1, $2, $3, $4, $5,
			(SELECT id FROM user_identities WHERE org_id = $2 AND member_id = $6 LIMIT 1))`,
		int64(id), int64(org), string(channel), channelUserID, handle, linkedBy)
	if isUniqueViolation(err) {
		return fmt.Errorf("store: %s/%s already speaks for a member of this org", channel, channelUserID)
	}
	if err != nil {
		return fmt.Errorf("store: link identity: %w", err)
	}
	return nil
}

// GrantRole gives a member an org role.
func (s *Store) GrantRole(ctx context.Context, org ledger.OrgID, id MemberID, role chat.Role, by MemberID) error {
	name := role.String()
	if role == chat.RoleNone {
		return errors.New("store: cannot grant the empty role")
	}
	var grantedBy *int64
	if by != 0 {
		v := int64(by)
		grantedBy = &v
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO member_roles (org_id, member_id, role, granted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, member_id, role) DO NOTHING`,
		int64(org), int64(id), name, grantedBy)
	if err != nil {
		return fmt.Errorf("store: grant %s: %w", name, err)
	}
	return nil
}

// RevokeRole removes an org role from a member.
func (s *Store) RevokeRole(ctx context.Context, org ledger.OrgID, id MemberID, role chat.Role) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM member_roles WHERE org_id = $1 AND member_id = $2 AND role = $3`,
		int64(org), int64(id), role.String())
	if err != nil {
		return fmt.Errorf("store: revoke %s: %w", role, err)
	}
	return nil
}

// EffectiveRole reports the highest role a member holds, counting both roles
// granted directly and roles conferred by a platform role they hold.
//
// platformRoles are the identifiers the platform reported for this space:
// Discord role snowflakes, or a Telegram membership status. They are matched
// against role_bindings, which by construction cannot confer admin.
//
// Worth restating where it is easiest to misread: this decides who the bot will
// talk to. It does not decide who can spend. That is settled by the network,
// against the treasury account's own signer list.
func (s *Store) EffectiveRole(
	ctx context.Context, org ledger.OrgID, id MemberID, channel chat.Channel, platformRoles []string,
) (chat.Role, error) {
	if platformRoles == nil {
		platformRoles = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT role FROM member_roles WHERE org_id = $1 AND member_id = $2
		UNION
		SELECT role FROM role_bindings
		 WHERE org_id = $1 AND channel = $3 AND channel_role = ANY($4)`,
		int64(org), int64(id), string(channel), platformRoles)
	if err != nil {
		return chat.RoleNone, fmt.Errorf("store: effective role: %w", err)
	}
	defer rows.Close()

	best := chat.RoleNone
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return chat.RoleNone, fmt.Errorf("store: scan role: %w", err)
		}
		if r := parseRole(name); r > best {
			best = r
		}
	}
	return best, rows.Err()
}

// BindRole makes a platform role confer an org role.
//
// Admin is refused here as well as by the schema. A guild administrator can
// create a role and assign it to themselves at will, so a binding that granted
// administration would make every guild admin a stelfin admin by construction.
func (s *Store) BindRole(
	ctx context.Context, org ledger.OrgID, channel chat.Channel, platformRole string, role chat.Role,
) error {
	switch {
	case !channel.Valid():
		return fmt.Errorf("store: unknown channel %q", channel)
	case platformRole == "":
		return errors.New("store: platform role is required")
	case role >= chat.RoleAdmin:
		return errors.New(
			"store: a platform role may not confer admin — anyone who can create " +
				"a role in the space could then grant themselves one")
	case role == chat.RoleNone:
		return errors.New("store: cannot bind the empty role")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO role_bindings (org_id, channel, channel_role, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`,
		int64(org), string(channel), platformRole, role.String())
	if err != nil {
		return fmt.Errorf("store: bind role: %w", err)
	}
	return nil
}

func parseRole(name string) chat.Role {
	switch name {
	case "observer":
		return chat.RoleObserver
	case "proposer":
		return chat.RoleProposer
	case "approver":
		return chat.RoleApprover
	case "admin":
		return chat.RoleAdmin
	default:
		return chat.RoleNone
	}
}
