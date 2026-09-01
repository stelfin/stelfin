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
	"github.com/stelfin/stelfin/ledger/store"
)

// Approve tokens authorise one member to add one signature to one proposal.
//
// The fourth of the near-identical token types, and versioned separately for
// the same reason as the others: the version is signed into the MAC, so even
// sharing a secret, none can be replayed as another.
//
// The important design decision here is that an approve token is still scoped
// to ONE person. A proposal needs several approvals, and the obvious shortcut
// is one shared link passed around the channel — which would make it a bearer
// credential that anyone who scrolled up can use, on a page that displays what
// a treasury is about to spend. Each approver asks for their own instead, and
// the reply carrying it is ephemeral like every other.
//
// What it actually grants is narrow: read this proposal, and offer a signature
// for it. The signature still has to verify against the account's own signers,
// so a stolen token cannot approve anything — it can only read. That is not a
// reason to widen it. It is the reason it is worth keeping narrow while it is
// still cheap.
const approveTokenVersion = "a1"

// ApproveTokens issues and verifies approve tokens.
type ApproveTokens struct {
	secret []byte
	// now is overridable in tests. Production leaves it nil and uses the clock.
	now func() time.Time
}

// NewApproveTokens returns an issuer keyed by secret.
func NewApproveTokens(secret []byte) (*ApproveTokens, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("api: approve token secret is %d bytes, want at least 32", len(secret))
	}
	return &ApproveTokens{secret: secret}, nil
}

func (c *ApproveTokens) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Issue mints a token authorising scope to approve one proposal, until
// expiresAt.
func (c *ApproveTokens) Issue(
	scope Scope, proposal store.ProposalID, expiresAt time.Time,
) (string, error) {
	if err := scope.check(); err != nil {
		return "", err
	}
	if proposal <= 0 {
		return "", errors.New("api: approve token needs a proposal")
	}

	payload := fmt.Sprintf("%d\x00%s\x00%d\x00%d",
		scope.Org, scope.OwnerRef, int64(proposal), expiresAt.UTC().Unix())
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return approveTokenVersion + "." + encoded + "." + c.sign(encoded), nil
}

// Verify checks a token and returns what it authorises.
//
// The signature is checked before the expiry is even read: an unauthentic token
// is rejected as invalid rather than leaking, through a differing error,
// whether its forged expiry happened to be in the future.
func (c *ApproveTokens) Verify(token string) (scope Scope, proposal store.ProposalID, err error) {
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != approveTokenVersion {
		return Scope{}, 0, fmt.Errorf("%w: unrecognised format", ErrTokenInvalid)
	}
	encoded, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return Scope{}, 0, fmt.Errorf("%w: missing signature", ErrTokenInvalid)
	}

	// Constant-time: a byte-by-byte comparison that returns early would let an
	// attacker recover a valid signature one byte at a time.
	if !hmac.Equal([]byte(mac), []byte(c.sign(encoded))) {
		return Scope{}, 0, fmt.Errorf("%w: signature does not match", ErrTokenInvalid)
	}

	payload, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if decodeErr != nil {
		return Scope{}, 0, fmt.Errorf("%w: undecodable payload", ErrTokenInvalid)
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 4 {
		return Scope{}, 0, fmt.Errorf("%w: malformed payload", ErrTokenInvalid)
	}

	org, orgErr := strconv.ParseInt(parts[0], 10, 64)
	if orgErr != nil || org == 0 {
		return Scope{}, 0, fmt.Errorf("%w: unreadable org", ErrTokenInvalid)
	}
	id, idErr := strconv.ParseInt(parts[2], 10, 64)
	if idErr != nil || id <= 0 {
		return Scope{}, 0, fmt.Errorf("%w: unreadable proposal", ErrTokenInvalid)
	}
	unix, convErr := strconv.ParseInt(parts[3], 10, 64)
	if convErr != nil {
		return Scope{}, 0, fmt.Errorf("%w: unreadable expiry", ErrTokenInvalid)
	}
	if !c.clock().Before(time.Unix(unix, 0)) {
		return Scope{}, 0, fmt.Errorf("%w: expired at %s", ErrTokenExpired, time.Unix(unix, 0).UTC())
	}

	return Scope{Org: ledger.OrgID(org), OwnerRef: parts[1]}, store.ProposalID(id), nil
}

func (c *ApproveTokens) sign(encoded string) string {
	mac := hmac.New(sha256.New, c.secret)
	// Version is signed too, so a token of one version can never be replayed as
	// another with different semantics.
	mac.Write([]byte(approveTokenVersion))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
