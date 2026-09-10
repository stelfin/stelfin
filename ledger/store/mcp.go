package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/ledger"
)

// Tiers an MCP token may reach.
const (
	// TierRead observes and nothing more.
	TierRead = "read"
	// TierPropose can additionally draft a payment for a human to approve. It
	// still cannot cause one.
	TierPropose = "propose"
)

// ErrNoToken reports a token that is unknown, revoked or expired.
//
// One error for all three: which it was tells a caller holding a rejected token
// something about the workspace, and none of it is information they can act on.
var ErrNoToken = errors.New("store: no live MCP token")

// tokenPrefix makes a leaked token recognisable in a log or a paste.
//
// Worth the four characters: a secret scanner, a code review or a person
// looking at a config file can tell what they have found, which is the
// difference between a rotated key and a key nobody noticed.
const tokenPrefix = "stlf_"

// MCPToken is a live token's record. Never the token itself.
type MCPToken struct {
	ID    int64
	Org   ledger.OrgID
	Label string
	Tier  string

	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Live reports whether this token may be used right now.
func (t MCPToken) Live(now time.Time) bool {
	switch {
	case t.RevokedAt != nil:
		return false
	case t.ExpiresAt != nil && !now.Before(*t.ExpiresAt):
		return false
	default:
		return true
	}
}

// IssueMCPToken mints a token and returns it once.
//
// The plaintext is returned here and stored nowhere. A workspace that loses it
// mints another; a database dump gives an attacker a list of what exists rather
// than a set of working keys.
func (s *Store) IssueMCPToken(
	ctx context.Context, org ledger.OrgID, label, tier string, by IdentityID,
	expiresAt *time.Time,
) (token string, record MCPToken, err error) {
	if strings.TrimSpace(label) == "" {
		return "", MCPToken{}, errors.New("store: a token needs a label to be revoked by")
	}
	if tier != TierRead && tier != TierPropose {
		return "", MCPToken{}, fmt.Errorf("store: %q is not a tier", tier)
	}

	// 32 bytes of randomness. The token is a bearer credential reaching a
	// workspace's whole history, so it is sized against guessing rather than
	// against convenience.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", MCPToken{}, fmt.Errorf("store: generate token: %w", err)
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	var createdBy any
	if by != 0 {
		createdBy = int64(by)
	}

	record, err = scanMCPToken(s.pool.QueryRow(ctx, `
		INSERT INTO mcp_tokens (org_id, token_hash, label, tier, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+mcpTokenColumns,
		int64(org), hashToken(token), strings.TrimSpace(label), tier, createdBy, expiresAt))
	if err != nil {
		return "", MCPToken{}, fmt.Errorf("store: issue MCP token: %w", err)
	}
	return token, record, nil
}

const mcpTokenColumns = `id, org_id, label, tier, created_at, last_used_at, expires_at, revoked_at`

func scanMCPToken(row pgx.Row) (MCPToken, error) {
	var t MCPToken
	err := row.Scan(&t.ID, &t.Org, &t.Label, &t.Tier,
		&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt)
	return t, err
}

// LookupMCPToken resolves a presented token, and records that it was used.
//
// The hash is computed from what was presented and matched by equality, which
// is a constant-time comparison inside Postgres against an indexed column
// rather than a scan — the timing question that matters here is answered by
// hashing before the query, not by how the query compares.
func (s *Store) LookupMCPToken(ctx context.Context, presented string) (MCPToken, error) {
	if !strings.HasPrefix(presented, tokenPrefix) {
		// Refused without a query. A token that is not one of ours should not
		// cost a database round trip, and saying so early is not a leak — the
		// prefix is visible in every token we hand out.
		return MCPToken{}, fmt.Errorf("%w: not a stelfin token", ErrNoToken)
	}

	record, err := scanMCPToken(s.pool.QueryRow(ctx, `
		UPDATE mcp_tokens SET last_used_at = now()
		 WHERE token_hash = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		RETURNING `+mcpTokenColumns,
		hashToken(presented)))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPToken{}, ErrNoToken
	}
	if err != nil {
		return MCPToken{}, fmt.Errorf("store: look up MCP token: %w", err)
	}
	return record, nil
}

// MCPTokens lists a workspace's tokens, newest first.
func (s *Store) MCPTokens(ctx context.Context, org ledger.OrgID) ([]MCPToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+mcpTokenColumns+` FROM mcp_tokens WHERE org_id = $1 ORDER BY created_at DESC`,
		int64(org))
	if err != nil {
		return nil, fmt.Errorf("store: list MCP tokens: %w", err)
	}
	defer rows.Close()

	var out []MCPToken
	for rows.Next() {
		t, err := scanMCPToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan MCP token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeMCPToken withdraws a token.
//
// The row is kept. "Revoked" and "never existed" are different answers to
// somebody asking why an integration stopped working.
func (s *Store) RevokeMCPToken(ctx context.Context, org ledger.OrgID, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE mcp_tokens SET revoked_at = now()
		 WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL`,
		id, int64(org))
	if err != nil {
		return fmt.Errorf("store: revoke MCP token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrNoToken, id)
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SameToken compares two tokens without leaking their difference through
// timing. Present for callers that hold two and must not branch on which.
func SameToken(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
