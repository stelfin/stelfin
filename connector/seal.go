package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
)

// Keeping a connector's credential.
//
// A Google service-account key, an MCP bearer token, an API secret: whatever a
// connector needs to reach the thing it connects to. These are the one class of
// secret this system stores rather than avoids, and they are stored because
// there is no alternative — an API key cannot be replaced by a signature the
// way a treasury key can.
//
// # Why the org is in the additional data
//
// The obvious design encrypts the credential and stores it in a row with an
// org id beside it. The failure of that design is a query: one missing
// predicate, one join written wrongly, one restore from a backup with a shifted
// sequence, and org A's request decrypts org B's credential — successfully,
// silently, with the correct plaintext.
//
// Binding the org into the AEAD's additional data makes that impossible rather
// than unlikely. A ciphertext moved to another tenant's row does not decrypt to
// something wrong; it does not decrypt at all. The database's predicate becomes
// a convenience, and the cryptography becomes the boundary.

var (
	// ErrSealed reports a credential that will not open.
	//
	// One error for a wrong key, a wrong org, a corrupted row and a truncated
	// one, because the distinction is not information the caller can act on and
	// telling them narrows a guess.
	ErrSealed = errors.New("connector: this credential cannot be opened")

	// ErrKeySize reports a sealing key that is not 32 bytes.
	ErrKeySize = errors.New("connector: a sealing key must be 32 bytes")
)

// sealVersion prefixes every ciphertext.
//
// So that a future change of cipher or of what goes into the additional data
// fails loudly on the first byte rather than as an authentication error nobody
// can explain.
const sealVersion = "s1"

// Sealer encrypts and decrypts connector credentials.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer returns a Sealer keyed by a 32-byte secret.
//
// The key belongs in a KMS and reaches this process as bytes. That is weaker
// than the treasury's arrangement and acceptable for a different reason: a
// leaked connector credential is a spreadsheet somebody can read, not a
// treasury somebody can drain.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w, got %d", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("connector: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("connector: gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts a credential for one connector in one org.
//
// Both the org and the connector id go into the additional data. The org stops
// a row crossing tenants; the connector id stops a credential granted for a
// read-only spreadsheet being presented as the one for a trading venue, which
// would otherwise be a matter of updating one column.
func (s *Sealer) Seal(org int64, connector string, credential []byte) (string, error) {
	if org <= 0 {
		return "", errors.New("connector: a credential must belong to an org")
	}
	if connector == "" {
		return "", errors.New("connector: a credential must name its connector")
	}

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("connector: nonce: %w", err)
	}

	sealed := s.aead.Seal(nonce, nonce, credential, additional(org, connector))
	return sealVersion + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a credential, for the org and connector it was sealed to.
//
// A ciphertext from another tenant's row does not open. That is the whole
// design: the failure is cryptographic rather than a predicate somebody
// forgot.
func (s *Sealer) Open(org int64, connector, sealed string) ([]byte, error) {
	version, encoded, ok := cut(sealed)
	if !ok || version != sealVersion {
		return nil, fmt.Errorf("%w: unrecognised format", ErrSealed)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable", ErrSealed)
	}
	if len(raw) < s.aead.NonceSize() {
		return nil, fmt.Errorf("%w: truncated", ErrSealed)
	}

	nonce, body := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, body, additional(org, connector))
	if err != nil {
		return nil, fmt.Errorf("%w: it was not sealed for this workspace and connector",
			ErrSealed)
	}
	return plain, nil
}

// additional is what the ciphertext is bound to.
//
// Length-prefixed rather than concatenated, so that org 1 with connector "23"
// and org 12 with connector "3" cannot produce the same additional data. A
// delimiter alone would not do it — a connector id containing the delimiter
// would collide — and a collision here is two tenants sharing a credential.
func additional(org int64, connector string) []byte {
	id := strconv.FormatInt(org, 10)
	out := make([]byte, 0, len(sealVersion)+len(id)+len(connector)+24)
	out = append(out, sealVersion...)
	out = appendField(out, id)
	out = appendField(out, connector)
	return out
}

func appendField(dst []byte, field string) []byte {
	dst = strconv.AppendInt(dst, int64(len(field)), 10)
	dst = append(dst, ':')
	return append(dst, field...)
}

func cut(s string) (version, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
