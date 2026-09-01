package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger"
)

// Org kinds.
const (
	// OrgPlatform is this deployment's own books: the operator's XLM float, the
	// reserves it carries for provisioned accounts, and the fees it pays.
	// Exactly one exists.
	OrgPlatform = "platform"
	// OrgDAO is a tenant.
	OrgDAO = "dao"
)

// Org statuses.
const (
	OrgActive    = "active"
	OrgSuspended = "suspended"
)

var (
	// ErrOrgNotFound reports an org that does not exist.
	ErrOrgNotFound = errors.New("store: org not found")
	// ErrSlugTaken reports an org slug already in use.
	ErrSlugTaken = errors.New("store: org slug is already taken")
	// ErrSpaceRegistered reports a space that already belongs to an org.
	ErrSpaceRegistered = errors.New("store: space already belongs to an org")
)

// slugPattern mirrors the schema's own CHECK, so a bad slug is a clear error
// here rather than a constraint violation from the driver.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// Org is one organisation.
type Org struct {
	ID          ledger.OrgID
	Kind        string
	Slug        string
	DisplayName string
	// Network is 'testnet' or 'public'. An org is bound to one for its
	// lifetime: a treasury address means nothing on the other one, and a mixed
	// org would let an approval signed against testnet be presented for a
	// mainnet envelope.
	Network string
	Status  string
	// ProposalTTL is how long a proposal may collect signatures.
	//
	// Per-org because the answer is a property of the group, not of the code: a
	// five-person treasury across three time zones is not the same signing
	// window as one person with a hardware wallet. It also has to bound the
	// envelope's own time bounds, or a proposal would outlive the transaction
	// it is collecting signatures for.
	ProposalTTL time.Duration
}

// Active reports whether this org may move money.
func (o Org) Active() bool { return o.Status == OrgActive }

const orgColumns = `id, kind, slug, display_name, network, status, proposal_ttl`

func scanOrg(row pgx.Row) (Org, error) {
	var o Org
	var ttl pgtype.Interval
	err := row.Scan(&o.ID, &o.Kind, &o.Slug, &o.DisplayName, &o.Network, &o.Status, &ttl)
	// An interval in months or days would be a policy this code has not been
	// asked to interpret; the column is CHECKed between one hour and seven days,
	// so anything else means the schema moved without this.
	o.ProposalTTL = time.Duration(ttl.Microseconds) * time.Microsecond
	return o, err
}

// EnsurePlatformOrg returns the deployment's own org, creating it if absent.
//
// Called at startup, for the same reason the native asset is registered there
// rather than in a migration: the network is configuration, and baking a
// network-specific row into a migration is how a testnet deployment ends up
// claiming to be mainnet.
//
// A network mismatch is fatal rather than corrected. If the process comes up
// pointed at a different network than the books were written against, the
// safest thing it can do is refuse to start.
func (s *Store) EnsurePlatformOrg(ctx context.Context, network string) (Org, error) {
	if network != "testnet" && network != "public" {
		return Org{}, fmt.Errorf("store: unknown network %q", network)
	}

	org, err := scanOrg(s.pool.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO orgs (kind, slug, display_name, network)
			VALUES ('platform', 'stelfin', 'stelfin', $1)
			ON CONFLICT (kind) WHERE kind = 'platform' DO NOTHING
			RETURNING `+orgColumns+`
		)
		SELECT `+orgColumns+` FROM inserted
		UNION ALL
		SELECT `+orgColumns+` FROM orgs WHERE kind = 'platform'
		LIMIT 1`, network))
	if err != nil {
		return Org{}, fmt.Errorf("store: ensure platform org: %w", err)
	}
	if org.Network != network {
		return Org{}, fmt.Errorf(
			"store: this database's platform org is on %q but the process is configured for %q; "+
				"refusing to start rather than mixing networks", org.Network, network)
	}
	return org, nil
}

// CreateOrgParams describes a new tenant.
type CreateOrgParams struct {
	Slug        string
	DisplayName string
	Network     string
	// Space is where the bot was installed. Registered in the same transaction
	// as the org, so a half-created tenant — an org nothing routes to, or a
	// space pointing at nothing — cannot exist.
	Channel chat.Channel
	SpaceID string
	// InstalledBy is the owner reference of whoever ran setup, for the audit
	// trail.
	InstalledBy string
}

// CreateOrg registers a tenant and the space it was installed in.
func (s *Store) CreateOrg(ctx context.Context, p CreateOrgParams) (Org, error) {
	slug := strings.ToLower(strings.TrimSpace(p.Slug))
	switch {
	case !slugPattern.MatchString(slug):
		return Org{}, fmt.Errorf("store: %q is not a usable org slug", p.Slug)
	case strings.TrimSpace(p.DisplayName) == "":
		return Org{}, errors.New("store: display name is required")
	case !p.Channel.Valid():
		return Org{}, fmt.Errorf("store: unknown channel %q", p.Channel)
	case p.SpaceID == "":
		return Org{}, errors.New("store: space id is required")
	case p.Network != "testnet" && p.Network != "public":
		return Org{}, fmt.Errorf("store: unknown network %q", p.Network)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Org{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	org, err := scanOrg(tx.QueryRow(ctx, `
		INSERT INTO orgs (kind, slug, display_name, network)
		VALUES ('dao', $1, $2, $3)
		RETURNING `+orgColumns, slug, p.DisplayName, p.Network))
	if err != nil {
		if isUniqueViolation(err) {
			return Org{}, fmt.Errorf("%w: %q", ErrSlugTaken, slug)
		}
		return Org{}, fmt.Errorf("store: create org: %w", err)
	}

	var claimed bool
	err = tx.QueryRow(ctx, `
		INSERT INTO org_spaces (org_id, channel, space_id, installed_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (channel, space_id) DO NOTHING
		RETURNING true`,
		int64(org.ID), string(p.Channel), p.SpaceID, p.InstalledBy,
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another org already owns this space. Rolling back takes the org with
		// it, which is what we want: a tenant with nowhere to be reached is
		// worse than no tenant.
		return Org{}, fmt.Errorf("%w: %s/%s", ErrSpaceRegistered, p.Channel, p.SpaceID)
	}
	if err != nil {
		return Org{}, fmt.Errorf("store: register space: %w", err)
	}

	// The counterparty accounts every org needs before it can record anything.
	// Created here so no later code path has to wonder whether they exist.
	for _, kind := range []ledger.AccountKind{
		ledger.AccountExternal, ledger.AccountFeeExpense,
		ledger.AccountSponsoredReserve, ledger.AccountTrading,
	} {
		if _, err := ensureAccountTx(ctx, tx, org.ID, kind, "", string(kind)); err != nil {
			return Org{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Org{}, fmt.Errorf("store: commit: %w", err)
	}
	return org, nil
}

// OrgForSpace resolves the tenant a message belongs to.
//
// This is the first thing done with an inbound message, before any of its
// content is interpreted. An unregistered space is not an error: the bot has
// been added somewhere nobody has run setup, and the correct response is
// silence rather than a reply to a room that never asked for one.
func (s *Store) OrgForSpace(ctx context.Context, channel chat.Channel, spaceID string) (Org, bool, error) {
	if !channel.Valid() || spaceID == "" {
		return Org{}, false, nil
	}
	org, err := scanOrg(s.pool.QueryRow(ctx, `
		SELECT `+prefixed(orgColumns, "o")+`
		  FROM org_spaces s
		  JOIN orgs o ON o.id = s.org_id
		 WHERE s.channel = $1 AND s.space_id = $2 AND s.active`,
		string(channel), spaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, false, nil
	}
	if err != nil {
		return Org{}, false, fmt.Errorf("store: org for space %s/%s: %w", channel, spaceID, err)
	}
	return org, true, nil
}

// Org returns one org by id.
func (s *Store) Org(ctx context.Context, id ledger.OrgID) (Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`SELECT `+orgColumns+` FROM orgs WHERE id = $1`, int64(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, fmt.Errorf("%w: %d", ErrOrgNotFound, id)
	}
	if err != nil {
		return Org{}, fmt.Errorf("store: org %d: %w", id, err)
	}
	return org, nil
}

// AddSpace registers another space against an existing org, so a DAO with both
// a Discord guild and a Telegram group is one tenant.
func (s *Store) AddSpace(ctx context.Context, org ledger.OrgID, channel chat.Channel, spaceID, by string) error {
	if !channel.Valid() {
		return fmt.Errorf("store: unknown channel %q", channel)
	}
	var claimed bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO org_spaces (org_id, channel, space_id, installed_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (channel, space_id) DO NOTHING
		RETURNING true`,
		int64(org), string(channel), spaceID, by,
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", ErrSpaceRegistered, channel, spaceID)
	}
	if err != nil {
		return fmt.Errorf("store: add space: %w", err)
	}
	return nil
}

// DeactivateSpace marks a space inactive, for when the bot is removed from it.
// The org and its books survive: being thrown out of one chat does not end a
// DAO's history.
func (s *Store) DeactivateSpace(ctx context.Context, channel chat.Channel, spaceID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE org_spaces SET active = false WHERE channel = $1 AND space_id = $2`,
		string(channel), spaceID)
	if err != nil {
		return fmt.Errorf("store: deactivate space: %w", err)
	}
	return nil
}

// prefixed qualifies a column list with a table alias.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ", ")
	for i, c := range parts {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}
