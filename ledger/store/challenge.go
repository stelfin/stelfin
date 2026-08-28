package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger"
)

// Challenge purposes.
const (
	// PurposeLinkMember binds a member to an address they control.
	PurposeLinkMember = "link_member"
	// PurposeLinkTreasury proves a group controls an org's treasury.
	PurposeLinkTreasury = "link_treasury"
)

var (
	// ErrNoChallenge reports a challenge that does not exist, has expired, or
	// has already been used. One error for all three: which it was is not
	// information the presenter needs, and telling them narrows a guess.
	ErrNoChallenge = errors.New("store: no live challenge")

	// ErrNoLinkCode reports a code that is unknown, expired or spent.
	ErrNoLinkCode = errors.New("store: no live link code")

	// ErrIdentityNotFound reports a chat identity with no row.
	ErrIdentityNotFound = errors.New("store: identity not found")
)

// IdentityID identifies one chat account within an org.
type IdentityID int64

// IdentityFor returns the row id of a chat identity, which a challenge is bound
// to.
func (s *Store) IdentityFor(
	ctx context.Context, org ledger.OrgID, channel chat.Channel, channelUserID string,
) (IdentityID, error) {
	var id IdentityID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM user_identities
		 WHERE org_id = $1 AND channel = $2 AND channel_user_id = $3`,
		int64(org), string(channel), channelUserID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s/%s", ErrIdentityNotFound, channel, channelUserID)
	}
	if err != nil {
		return 0, fmt.Errorf("store: identity for %s/%s: %w", channel, channelUserID, err)
	}
	return id, nil
}

// Challenge is a SEP-10 challenge awaiting a signature.
type Challenge struct {
	Hash      string
	Org       ledger.OrgID
	Identity  IdentityID
	Purpose   string
	Address   string
	XDR       string
	ExpiresAt time.Time
}

// SaveChallenge records a challenge, superseding any live one for the same
// identity and purpose.
//
// Superseding rather than refusing: asking again is what someone does when the
// first link expired in a scrollback, and leaving orphans behind would let an
// old challenge be completed after a new one was issued.
func (s *Store) SaveChallenge(ctx context.Context, c Challenge) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM auth_challenges
		 WHERE identity_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		int64(c.Identity), c.Purpose,
	); err != nil {
		return fmt.Errorf("store: supersede challenge: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_challenges
			(hash, org_id, identity_id, purpose, address, envelope_xdr, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.Hash, int64(c.Org), int64(c.Identity), c.Purpose, c.Address, c.XDR, c.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store: save challenge: %w", err)
	}
	return tx.Commit(ctx)
}

// LiveChallenge returns an unconsumed, unexpired challenge by hash.
func (s *Store) LiveChallenge(ctx context.Context, org ledger.OrgID, hash string) (Challenge, error) {
	var c Challenge
	err := s.pool.QueryRow(ctx, `
		SELECT hash, org_id, identity_id, purpose, address, envelope_xdr, expires_at
		  FROM auth_challenges
		 WHERE hash = $1 AND org_id = $2 AND consumed_at IS NULL AND expires_at > now()`,
		hash, int64(org),
	).Scan(&c.Hash, &c.Org, &c.Identity, &c.Purpose, &c.Address, &c.XDR, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("store: challenge %s: %w", hash, err)
	}
	return c, nil
}

// ConsumeChallenge claims a challenge and binds the address to the member the
// issuing identity speaks for, in one transaction.
//
// Conditional UPDATE rather than read-then-write, so two submissions of the
// same signed challenge cannot both succeed. The address binding lives in the
// same transaction because a consumed challenge with no address recorded would
// be an authorisation spent for nothing, and there is no way to ask for it back.
func (s *Store) ConsumeChallenge(
	ctx context.Context, org ledger.OrgID, hash, address, source string,
) (MemberID, error) {
	if source != AddressLinked && source != AddressProvisioned {
		return 0, fmt.Errorf("store: unknown address source %q", source)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var identity IdentityID
	err = tx.QueryRow(ctx, `
		UPDATE auth_challenges
		   SET consumed_at = now()
		 WHERE hash = $1
		   AND org_id = $2
		   AND address = $3
		   AND consumed_at IS NULL
		   AND expires_at > now()
		RETURNING identity_id`,
		hash, int64(org), address,
	).Scan(&identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return 0, fmt.Errorf("store: consume challenge %s: %w", hash, err)
	}

	var member MemberID
	if err := tx.QueryRow(ctx,
		`SELECT member_id FROM user_identities WHERE id = $1 AND org_id = $2`,
		int64(identity), int64(org),
	).Scan(&member); err != nil {
		return 0, fmt.Errorf("store: member for identity %d: %w", identity, err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE members
		   SET address = $3, address_source = $4, address_verified_at = now()
		 WHERE org_id = $1 AND id = $2`,
		int64(org), int64(member), address, source)
	if isUniqueViolation(err) {
		return 0, fmt.Errorf("%w: %s", ErrAddressTaken, address)
	}
	if err != nil {
		return 0, fmt.Errorf("store: bind address: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("%w: %d", ErrMemberNotFound, member)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return member, nil
}

// linkCodeAlphabet excludes characters that are read back wrongly: no 0/O, no
// 1/I/L. A code is meant to be typed from a screen into another device.
const linkCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// LinkCodeLength is the length of a link code.
//
// Eight characters from a 31-character alphabet is about 39 bits. Guessing one
// inside its ten-minute life is not a realistic attack, and the code cannot
// create or change an address in any case — the worst it does is let someone
// speak as a member who still cannot sign anything.
const LinkCodeLength = 8

// IssueLinkCode mints a code that attaches another chat account to a member,
// superseding any live one.
func (s *Store) IssueLinkCode(
	ctx context.Context, org ledger.OrgID, member MemberID, by IdentityID, ttl time.Duration,
) (string, error) {
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM identity_link_codes WHERE member_id = $1 AND consumed_at IS NULL`,
		int64(member),
	); err != nil {
		return "", fmt.Errorf("store: supersede link code: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_link_codes (code, org_id, member_id, issued_by, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)`,
		code, int64(org), int64(member), int64(by), ttl.String(),
	); err != nil {
		return "", fmt.Errorf("store: issue link code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit: %w", err)
	}
	return code, nil
}

// ClaimLinkCode spends a code and points a chat account at the member it names.
//
// Points, not inserts. By the time anyone runs this, the claiming account has
// already spoken once and therefore already has a member of its own — an empty
// one, with no address and no roles, created the moment it was first seen. What
// the code does is merge that away: the identity moves to the member that
// issued the code, and the husk it left behind is deleted.
//
// The husk is only deleted when it is genuinely empty. A member with an address
// or a role is somebody, and quietly removing one because a code was typed
// would be a way to erase a person rather than to link them.
//
// The claim is a conditional UPDATE, so two people racing on one code cannot
// both win, and everything happens in one transaction: a spent code that
// attached nothing would be unrecoverable, because the code is gone.
func (s *Store) ClaimLinkCode(
	ctx context.Context, org ledger.OrgID, code string,
	channel chat.Channel, channelUserID, handle string,
) (MemberID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var target MemberID
	var issuedBy IdentityID
	err = tx.QueryRow(ctx, `
		UPDATE identity_link_codes
		   SET consumed_at = now()
		 WHERE code = $1
		   AND org_id = $2
		   AND consumed_at IS NULL
		   AND expires_at > now()
		RETURNING member_id, issued_by`,
		code, int64(org),
	).Scan(&target, &issuedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoLinkCode
	}
	if err != nil {
		return 0, fmt.Errorf("store: claim link code: %w", err)
	}

	// What this account currently speaks for, if anything.
	var identity IdentityID
	var previous MemberID
	var previousAddress *string
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.member_id, m.address
		  FROM user_identities i
		  JOIN members m ON m.id = i.member_id AND m.org_id = i.org_id
		 WHERE i.org_id = $1 AND i.channel = $2 AND i.channel_user_id = $3`,
		int64(org), string(channel), channelUserID,
	).Scan(&identity, &previous, &previousAddress)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Never spoken here before. Straightforward insert.
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_identities (member_id, org_id, channel, channel_user_id, handle, linked_by)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			int64(target), int64(org), string(channel), channelUserID, handle, int64(issuedBy),
		); err != nil {
			return 0, fmt.Errorf("store: attach identity: %w", err)
		}

	case err != nil:
		return 0, fmt.Errorf("store: read current identity: %w", err)

	case previous == target:
		// Already the right member. Spending the code changed nothing, which is
		// not an error — someone claimed their own code.

	case previousAddress != nil:
		// This account speaks for somebody who has proved an address. Moving it
		// would take a proved member's handle away on the strength of a shared
		// secret, which is exactly what the code is not allowed to do.
		return 0, fmt.Errorf(
			"store: %s/%s already speaks for a member with a linked wallet", channel, channelUserID)

	default:
		if _, err := tx.Exec(ctx, `
			UPDATE user_identities
			   SET member_id = $1, handle = $2, linked_by = $3
			 WHERE id = $4`,
			int64(target), handle, int64(issuedBy), int64(identity),
		); err != nil {
			return 0, fmt.Errorf("store: move identity: %w", err)
		}
		if err := deleteEmptyMember(ctx, tx, org, previous); err != nil {
			return 0, err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE identity_link_codes SET consumed_by =
			(SELECT id FROM user_identities
			  WHERE org_id = $1 AND channel = $2 AND channel_user_id = $3)
		  WHERE code = $4`,
		int64(org), string(channel), channelUserID, code,
	); err != nil {
		return 0, fmt.Errorf("store: record who claimed the code: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return target, nil
}

// deleteEmptyMember removes a member that nothing points at any more.
//
// Guarded on every count: an address, a role, or another identity all mean this
// is somebody, and the predicates are in the statement rather than checked
// first so a concurrent grant cannot slip between the two.
func deleteEmptyMember(ctx context.Context, q querier, org ledger.OrgID, member MemberID) error {
	_, err := q.Exec(ctx, `
		DELETE FROM members m
		 WHERE m.id = $1
		   AND m.org_id = $2
		   AND m.address IS NULL
		   AND NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.member_id = m.id)
		   AND NOT EXISTS (SELECT 1 FROM member_roles r WHERE r.member_id = m.id)`,
		int64(member), int64(org))
	if err != nil {
		return fmt.Errorf("store: remove the emptied member: %w", err)
	}
	return nil
}

// newLinkCode returns a random code from the reduced alphabet.
//
// crypto/rand, not math/rand: the code is a bearer credential for the ten
// minutes it lives, and a predictable one would be guessable by anyone who saw
// a previous code.
func newLinkCode() (string, error) {
	out := make([]byte, LinkCodeLength)
	max := big.NewInt(int64(len(linkCodeAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("store: generate link code: %w", err)
		}
		out[i] = linkCodeAlphabet[n.Int64()]
	}
	return string(out), nil
}
