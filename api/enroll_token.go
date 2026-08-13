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

// Enroll tokens authorise one phone number to create its Stellar account.
//
// Structurally identical to ConfirmTokens — same fragment-borne, stateless,
// expiring HMAC scheme — but versioned separately ("e1" instead of "v1") so
// that even sharing a secret with ConfirmTokens, an enroll token can never be
// replayed as authority to submit a payment, or a confirm token replayed as
// authority to create an account. The version is signed into the MAC, so a
// forged version byte fails verification rather than being read as a claim.
//
// Kept as a distinct type rather than a shared abstraction with ConfirmTokens
// deliberately: the two authorise different, non-overlapping things, and
// duplicating ~90 lines of well-tested code costs less than the risk of a
// shared type's future change quietly widening what one token can do.

const enrollTokenVersion = "e1"

// EnrollTokens issues and verifies enrollment tokens.
type EnrollTokens struct {
	secret []byte
	// now is overridable in tests. Production leaves it nil and uses the clock.
	now func() time.Time
}

// NewEnrollTokens returns an issuer keyed by secret. The same secret used for
// ConfirmTokens is fine — see the version-separation note above.
func NewEnrollTokens(secret []byte) (*EnrollTokens, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("api: enroll token secret is %d bytes, want at least 32", len(secret))
	}
	return &EnrollTokens{secret: secret}, nil
}

func (c *EnrollTokens) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Issue mints a token authorising ownerRef to enroll, until expiresAt.
func (c *EnrollTokens) Issue(ownerRef string, expiresAt time.Time) (string, error) {
	if ownerRef == "" {
		return "", errors.New("api: enroll token needs an owner")
	}
	if strings.ContainsRune(ownerRef, 0) {
		return "", errors.New("api: enroll token owner must not contain NUL")
	}

	payload := fmt.Sprintf("%s\x00%d", ownerRef, expiresAt.UTC().Unix())
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enrollTokenVersion + "." + encoded + "." + c.sign(encoded), nil
}

// Verify checks a token and returns the owner it authorises.
func (c *EnrollTokens) Verify(token string) (ownerRef string, err error) {
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != enrollTokenVersion {
		return "", fmt.Errorf("%w: unrecognised format", ErrTokenInvalid)
	}
	encoded, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return "", fmt.Errorf("%w: missing signature", ErrTokenInvalid)
	}

	if !hmac.Equal([]byte(mac), []byte(c.sign(encoded))) {
		return "", fmt.Errorf("%w: signature does not match", ErrTokenInvalid)
	}

	payload, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if decodeErr != nil {
		return "", fmt.Errorf("%w: undecodable payload", ErrTokenInvalid)
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: malformed payload", ErrTokenInvalid)
	}

	unix, convErr := strconv.ParseInt(parts[1], 10, 64)
	if convErr != nil {
		return "", fmt.Errorf("%w: unreadable expiry", ErrTokenInvalid)
	}
	if !c.clock().Before(time.Unix(unix, 0)) {
		return "", fmt.Errorf("%w: expired at %s", ErrTokenExpired, time.Unix(unix, 0).UTC())
	}

	return parts[0], nil
}

func (c *EnrollTokens) sign(encoded string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(enrollTokenVersion))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
