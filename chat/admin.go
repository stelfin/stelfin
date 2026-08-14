package chat

import "context"

// AdminChecker reports platform-level administrative authority over a space.
//
// It is a separate, optional interface rather than a field on Actor because the
// two platforms answer the question in fundamentally different ways, and the
// difference is a security property rather than an implementation detail.
//
// Discord signs the interaction body with Ed25519, and that body carries the
// member's permission bits — so the answer arrives with the message, already
// authenticated, and Parse can set Actor.IsSpaceAdmin directly.
//
// Telegram authenticates a delivery with a shared secret echoed in a header,
// which proves the caller knew the secret and says nothing about the body. A
// claim of administrative status *inside* a Telegram update is therefore worth
// nothing, and the answer has to be fetched from the platform's own API. That
// is a network call, so it cannot happen inside Parse, which must stay pure.
//
// Callers ask only when a command actually requires it. Implementations may
// cache briefly; the cost of a slightly stale answer is that someone demoted
// seconds ago can still run one administrative command, which is bounded, while
// the cost of asking on every message is a round trip per message.
type AdminChecker interface {
	IsSpaceAdmin(ctx context.Context, spaceID, userID string) (bool, error)
}
