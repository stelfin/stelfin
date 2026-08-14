package chat

import (
	"errors"
	"io"
	"net/http"
)

// MaxWebhookBody bounds a webhook read.
//
// Applied before authentication, deliberately. A body is attacker-controlled
// until it has been verified, and an unbounded read of attacker-controlled
// input is a denial of service whatever the signature turns out to say.
const MaxWebhookBody int64 = 1 << 20 // 1 MiB

// ErrUnauthenticated reports a request that did not prove it came from the
// platform it claims to be.
//
// One error for every failure mode on purpose. Which check failed — a missing
// header, a malformed signature, a stale timestamp — is information an
// unauthenticated caller has no business learning, so the distinction stays in
// the log.
var ErrUnauthenticated = errors.New("chat: request is not from the claimed platform")

// ReadLimited reads a request body up to MaxWebhookBody and returns the exact
// bytes read.
//
// Every Transport.Verify uses this and nothing parses before it. The bytes are
// what a signature covers: decoding JSON and re-encoding it to check a MAC
// changes them, and a check over different bytes than the ones that were signed
// is not a check.
func ReadLimited(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("chat: request has no body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody))
	if err != nil {
		return nil, err
	}
	return body, nil
}
