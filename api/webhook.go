package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Webhook signature verification.
//
// The webhook endpoint is the one part of this service reachable by anyone on
// the internet, and a message arriving through it can start a payment flow. It
// must therefore establish that Meta actually sent the request before anything
// downstream reads a byte of it.
//
// Two details do most of the work, and both are easy to get wrong:
//
//   - The MAC covers the *raw* request body. Decoding the JSON and re-encoding
//     it to verify changes the bytes — key order, whitespace, number
//     formatting — and the signature stops matching. Read the body once, verify
//     those exact bytes, then parse.
//   - The comparison is constant-time. A byte-by-byte compare that returns on
//     first mismatch leaks how much of a guess was right, which is enough to
//     recover a valid signature one byte at a time.

// MaxWebhookBody bounds how much will be read from a webhook request. Without
// it, an unauthenticated caller could stream indefinitely and exhaust memory
// before the signature is ever checked.
const MaxWebhookBody int64 = 1 << 20 // 1 MiB

var (
	// ErrSignatureMissing reports a request with no signature header.
	ErrSignatureMissing = errors.New("api: request carries no signature")

	// ErrSignatureInvalid reports a request whose signature does not match.
	ErrSignatureInvalid = errors.New("api: request signature does not match")
)

// VerifySignature checks Meta's X-Hub-Signature-256 header against the body.
//
// body must be the exact bytes received.
func VerifySignature(appSecret []byte, header string, body []byte) error {
	if header == "" {
		return ErrSignatureMissing
	}
	// Header form is "sha256=<hex>". Anything else is rejected rather than
	// guessed at — accepting a bare hex digest would also accept "sha1=..."
	// from a downgraded sender.
	algorithm, digest, ok := strings.Cut(header, "=")
	if !ok || algorithm != "sha256" {
		return fmt.Errorf("%w: unrecognised signature format", ErrSignatureInvalid)
	}
	provided, err := hex.DecodeString(digest)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrSignatureInvalid)
	}

	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return ErrSignatureInvalid
	}
	return nil
}

// ReadVerifiedBody reads a webhook request body and verifies its signature.
//
// It returns the raw bytes so the caller can parse them itself — the verified
// bytes and the parsed bytes must be the same ones.
func ReadVerifiedBody(appSecret []byte, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil {
		return nil, fmt.Errorf("api: read webhook body: %w", err)
	}
	if int64(len(body)) > MaxWebhookBody {
		return nil, fmt.Errorf("api: webhook body exceeds %d bytes", MaxWebhookBody)
	}
	if err := VerifySignature(appSecret, r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		return nil, err
	}
	return body, nil
}

// VerifyChallenge answers Meta's subscription handshake.
//
// Meta issues a GET carrying a token it was configured with; echoing the
// challenge back proves this endpoint holds the same token. The comparison is
// constant-time for the same reason the signature check is.
func VerifyChallenge(verifyToken string, query map[string][]string) (challenge string, err error) {
	first := func(key string) string {
		if v := query[key]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	if first("hub.mode") != "subscribe" {
		return "", errors.New("api: not a subscription challenge")
	}
	provided := first("hub.verify_token")
	if !hmac.Equal([]byte(provided), []byte(verifyToken)) {
		return "", fmt.Errorf("%w: verify token does not match", ErrSignatureInvalid)
	}
	c := first("hub.challenge")
	if c == "" {
		return "", errors.New("api: challenge is empty")
	}
	return c, nil
}
