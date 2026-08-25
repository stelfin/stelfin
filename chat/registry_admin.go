package chat

import (
	"context"
	"fmt"
)

// IsSpaceAdmin reports whether an actor administers the space a message arrived
// in.
//
// Which source the answer comes from depends on the platform, and the choice is
// a security one rather than an implementation detail.
//
// A transport that implements AdminChecker is asked, because its deliveries do
// not authenticate their own contents — Telegram echoes a shared secret in a
// header, so a caller who has that secret could claim anything, and a privilege
// claim inside the body is worth nothing.
//
// A transport that does not implement it has already answered: Discord signs
// the interaction body with Ed25519, so the member's permission bits arrived
// authenticated and Parse set the flag from them.
//
// The default when neither applies is false. Refusing an administrative command
// because the answer is unavailable is recoverable; allowing one is not.
func (r *Registry) IsSpaceAdmin(ctx context.Context, to Conversation, actor Actor) (bool, error) {
	t, ok := r.byChannel[to.Channel]
	if !ok {
		return false, fmt.Errorf("chat: no transport for channel %q", to.Channel)
	}
	if checker, ok := t.(AdminChecker); ok {
		return checker.IsSpaceAdmin(ctx, to.SpaceID, actor.UserID)
	}
	return actor.IsSpaceAdmin, nil
}
