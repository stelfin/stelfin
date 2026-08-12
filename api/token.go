package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Confirm tokens authorise one user to sign and submit one transaction.
//
// A user arrives at the confirmation page from a WhatsApp link, with no
// session and no cookie. The link itself has to carry the authority — so it is
// scoped as narrowly as the job allows: a token names exactly one transaction
// hash and expires with it. A leaked link cannot be used to send a different
// payment, list the user's history, or do anything at all once the transaction
// it names has expired.
//
// Tokens are stateless: the signature is the proof, so no lookup table can
// drift out of sync with the transactions it guards.
//
// The token belongs in the URL *fragment* (`/confirm#<token>`), not the query
// string. Fragments are not sent to the server on page load and do not appear
// in access logs, proxy logs, or Referer headers.

const tokenVersion = "v1"

var (
	// ErrTokenInvalid reports a token that is malformed or not authentic.
	ErrTokenInvalid = errors.New("api: confirmation token is not valid")

	// ErrTokenExpired reports an authentic token past its expiry.
	ErrTokenExpired = errors.New("api: confirmation token has expired")
)

// ConfirmTokens issues and verifies confirmation tokens.
type ConfirmTokens struct {
	secret []byte
	// now is overridable in tests. Production leaves it nil and uses the clock.
	now func() time.Time
}

// NewConfirmTokens returns an issuer keyed by secret.
//
// The secret must be at least 32 bytes of random data and must not be derived
// from anything guessable: it is the only thing standing between a stranger and
// the authority to submit another user's payment.
func NewConfirmTokens(secret []byte) (*ConfirmTokens, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("api: confirmation token secret is %d bytes, want at least 32", len(secret))
	}
	return &ConfirmTokens{secret: secret}, nil
}

func (c *ConfirmTokens) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Issue mints a token authorising ownerRef to submit the transaction named by
// hash, until expiresAt.
func (c *ConfirmTokens) Issue(ownerRef, hash string, expiresAt time.Time) (string, error) {
	if ownerRef == "" || hash == "" {
		return "", errors.New("api: confirmation token needs an owner and a hash")
	}
	// The payload is joined with a byte that cannot occur in either field, so
	// no combination of owner and hash can be re-split into a different pair.
	if strings.ContainsRune(ownerRef, 0) || strings.ContainsRune(hash, 0) {
		return "", errors.New("api: confirmation token fields must not contain NUL")
	}

	payload := fmt.Sprintf("%s\x00%s\x00%d", ownerRef, hash, expiresAt.UTC().Unix())
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return tokenVersion + "." + encoded + "." + c.sign(encoded), nil
}

// Verify checks a token and returns what it authorises.
//
// The signature is checked before the expiry is even read: an unauthentic
// token is rejected as invalid rather than leaking, through a differing error,
// whether its forged expiry happened to be in the future.
func (c *ConfirmTokens) Verify(token string) (ownerRef, hash string, err error) {
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != tokenVersion {
		return "", "", fmt.Errorf("%w: unrecognised format", ErrTokenInvalid)
	}
	encoded, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return "", "", fmt.Errorf("%w: missing signature", ErrTokenInvalid)
	}

	// Constant-time: a byte-by-byte comparison that returns early would let an
	// attacker recover a valid signature one byte at a time.
	if !hmac.Equal([]byte(mac), []byte(c.sign(encoded))) {
		return "", "", fmt.Errorf("%w: signature does not match", ErrTokenInvalid)
	}

	payload, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if decodeErr != nil {
		return "", "", fmt.Errorf("%w: undecodable payload", ErrTokenInvalid)
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("%w: malformed payload", ErrTokenInvalid)
	}

	unix, convErr := strconv.ParseInt(parts[2], 10, 64)
	if convErr != nil {
		return "", "", fmt.Errorf("%w: unreadable expiry", ErrTokenInvalid)
	}
	if !c.clock().Before(time.Unix(unix, 0)) {
		return "", "", fmt.Errorf("%w: expired at %s", ErrTokenExpired, time.Unix(unix, 0).UTC())
	}

	return parts[0], parts[1], nil
}

func (c *ConfirmTokens) sign(encoded string) string {
	mac := hmac.New(sha256.New, c.secret)
	// Version is signed too, so a future v2 token can never be replayed as a
	// v1 one with different semantics.
	mac.Write([]byte(tokenVersion))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
