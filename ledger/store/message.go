package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ClaimMessage records a delivery id, reporting whether this caller won it.
//
// An insert that either succeeds or conflicts, rather than a read followed by a
// write: two concurrent retries of the same delivery cannot both proceed.
// Platforms retry anything they consider slow or failed, and one instruction
// becoming two confirmations is one tap away from two payments.
//
// Deliberately not org-scoped. The id is the platform's own, prefixed by
// channel, so it is unique across the deployment; scoping it would only create
// a way for one delivery to be processed twice.
func (s *Store) ClaimMessage(ctx context.Context, dedupeID, sender string) (bool, error) {
	if dedupeID == "" {
		return false, errors.New("store: cannot claim a message with no id")
	}
	var claimed bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO processed_messages (id, sender)
		VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING
		RETURNING true`,
		dedupeID, sender,
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflict path: another delivery of this message already claimed it.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: claim message %s: %w", dedupeID, err)
	}
	return true, nil
}
