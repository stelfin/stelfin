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

	"github.com/stelfin/stelfin/ledger"
)

// Link tokens authorise one member to complete one SEP-10 challenge.
//
// Same shape as ConfirmTokens, and versioned separately so that even sharing a
// secret with it, neither can ever be replayed as the other. The version is
// signed into the MAC, so a forged version byte fails verification rather than
// being read as a claim.
//
// Three near-identical token types rather than one generic one, deliberately.
// They authorise different, non-overlapping things, and duplicating ninety
// lines of well-tested code costs less than the risk of a shared type's future
// change quietly widening what one of them can do.
//
// This is the least dangerous of the three — it grants the right to sign a
// transaction built with sequence number zero, which can never be submitted —
// and it is still scoped as narrowly as the others, because the day someone
// widens it is the day that stops being true.
//
// Fragment-borne, like the others: `/link#<token>` is not sent to the server on
// page load and does not appear in access logs, proxy logs or Referer headers.

// v2 carries the org. v1 did not, because there was only ever one: with
// several, a token naming an owner alone would authorise whichever org's row
// happened to be found. The version is signed into the MAC, so a v1 token can
// never be replayed as a v2 one with the org silently defaulted.
const linkTokenVersion = "l1"

// LinkTokens issues and verifies link tokens.
type LinkTokens struct {
	secret []byte
	// now is overridable in tests. Production leaves it nil and uses the clock.
	now func() time.Time
}

// NewLinkTokens returns an issuer keyed by secret.
//
// The secret must be at least 32 bytes of random data and must not be derived
// from anything guessable: it is the only thing standing between a stranger and
// the authority to submit another user's payment.
func NewLinkTokens(secret []byte) (*LinkTokens, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("api: link token secret is %d bytes, want at least 32", len(secret))
	}
	return &LinkTokens{secret: secret}, nil
}

func (c *LinkTokens) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Issue mints a token authorising scope to submit the transaction named by
// hash, until expiresAt.
func (c *LinkTokens) Issue(scope Scope, hash string, expiresAt time.Time) (string, error) {
	if err := scope.check(); err != nil {
		return "", err
	}
	if hash == "" {
		return "", errors.New("api: link token needs a hash")
	}
	// The payload is joined with a byte that cannot occur in any field, so no
	// combination of values can be re-split into a different one.
	if strings.ContainsRune(hash, 0) {
		return "", errors.New("api: link token fields must not contain NUL")
	}

	payload := fmt.Sprintf("%d\x00%s\x00%s\x00%d",
		scope.Org, scope.OwnerRef, hash, expiresAt.UTC().Unix())
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return linkTokenVersion + "." + encoded + "." + c.sign(encoded), nil
}

// Verify checks a token and returns what it authorises.
//
// The signature is checked before the expiry is even read: an unauthentic
// token is rejected as invalid rather than leaking, through a differing error,
// whether its forged expiry happened to be in the future.
func (c *LinkTokens) Verify(token string) (scope Scope, hash string, err error) {
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != linkTokenVersion {
		return Scope{}, "", fmt.Errorf("%w: unrecognised format", ErrTokenInvalid)
	}
	encoded, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return Scope{}, "", fmt.Errorf("%w: missing signature", ErrTokenInvalid)
	}

	// Constant-time: a byte-by-byte comparison that returns early would let an
	// attacker recover a valid signature one byte at a time.
	if !hmac.Equal([]byte(mac), []byte(c.sign(encoded))) {
		return Scope{}, "", fmt.Errorf("%w: signature does not match", ErrTokenInvalid)
	}

	payload, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if decodeErr != nil {
		return Scope{}, "", fmt.Errorf("%w: undecodable payload", ErrTokenInvalid)
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 4 {
		return Scope{}, "", fmt.Errorf("%w: malformed payload", ErrTokenInvalid)
	}

	org, orgErr := strconv.ParseInt(parts[0], 10, 64)
	if orgErr != nil || org == 0 {
		return Scope{}, "", fmt.Errorf("%w: unreadable org", ErrTokenInvalid)
	}
	unix, convErr := strconv.ParseInt(parts[3], 10, 64)
	if convErr != nil {
		return Scope{}, "", fmt.Errorf("%w: unreadable expiry", ErrTokenInvalid)
	}
	if !c.clock().Before(time.Unix(unix, 0)) {
		return Scope{}, "", fmt.Errorf("%w: expired at %s", ErrTokenExpired, time.Unix(unix, 0).UTC())
	}

	return Scope{Org: ledger.OrgID(org), OwnerRef: parts[1]}, parts[2], nil
}

func (c *LinkTokens) sign(encoded string) string {
	mac := hmac.New(sha256.New, c.secret)
	// Version is signed too, so a token of one version can never be replayed
	// as another with different semantics.
	mac.Write([]byte(linkTokenVersion))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
