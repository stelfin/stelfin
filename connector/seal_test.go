package connector_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stelfin/stelfin/connector"
)

func newSealer(t *testing.T) *connector.Sealer {
	t.Helper()
	s, err := connector.NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	return s
}

func TestSealRoundTrip(t *testing.T) {
	s := newSealer(t)
	secret := []byte(`{"type":"service_account","private_key":"..."}`)

	sealed, err := s.Seal(42, "sheets", secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// The plaintext must not be recoverable by reading the row.
	if bytes.Contains([]byte(sealed), []byte("private_key")) {
		t.Fatal("the sealed form contains the plaintext")
	}

	got, err := s.Open(42, "sheets", sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("opened %q", got)
	}
}

// TestACredentialMovedBetweenOrgsDecryptsToNothing is the property the whole
// design exists for.
//
// The obvious alternative stores the ciphertext beside an org id and trusts a
// predicate. One missing WHERE, one join written wrongly, one restore with a
// shifted sequence, and org A decrypts org B's credential successfully and
// silently. Here it does not decrypt at all.
func TestACredentialMovedBetweenOrgsDecryptsToNothing(t *testing.T) {
	s := newSealer(t)
	sealed, err := s.Seal(42, "sheets", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := s.Open(43, "sheets", sealed); !errors.Is(err, connector.ErrSealed) {
		t.Fatalf("another org opened it: %v", err)
	}
	// And the one it belongs to still can, so the check is binding rather than
	// simply breaking everything.
	if _, err := s.Open(42, "sheets", sealed); err != nil {
		t.Fatalf("the owning org could not open it: %v", err)
	}
}

// TestACredentialCannotBePresentedAsAnothersConnector: without this, promoting
// a read-only spreadsheet's credential to a trading connector's is a matter of
// updating one column.
func TestACredentialCannotBePresentedAsAnothersConnector(t *testing.T) {
	s := newSealer(t)
	sealed, err := s.Seal(42, "sheets", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := s.Open(42, "trading", sealed); !errors.Is(err, connector.ErrSealed) {
		t.Fatalf("it opened as another connector: %v", err)
	}
}

// TestTheBindingCannotCollide: length-prefixed rather than concatenated, so
// org 1 with connector "23" and org 12 with connector "3" bind differently. A
// collision here is two tenants sharing a credential.
func TestTheBindingCannotCollide(t *testing.T) {
	s := newSealer(t)
	sealed, err := s.Seal(1, "23", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s.Open(12, "3", sealed); !errors.Is(err, connector.ErrSealed) {
		t.Fatalf("a differently-split org and connector opened it: %v", err)
	}
}

func TestTamperingIsRefused(t *testing.T) {
	s := newSealer(t)
	sealed, err := s.Seal(42, "sheets", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Flipped in the middle of the ciphertext, not at the end: the last
	// base64 character carries unused bits, so changing it can decode to the
	// same bytes and prove nothing.
	flipped := []byte(sealed)
	at := len(flipped) / 2
	if flipped[at] == 'A' {
		flipped[at] = 'B'
	} else {
		flipped[at] = 'A'
	}

	for name, bad := range map[string]string{
		"a flipped byte":      string(flipped),
		"a different version": "s2" + sealed[2:],
		"no version":          sealed[3:],
		"empty":               "",
		"truncated":           sealed[:6],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Open(42, "sheets", bad); !errors.Is(err, connector.ErrSealed) {
				t.Fatalf("error = %v, want ErrSealed", err)
			}
		})
	}
}

// TestSealingIsNotDeterministic: two seals of one secret must differ, or the
// database leaks which orgs share a credential.
func TestSealingIsNotDeterministic(t *testing.T) {
	s := newSealer(t)
	first, err := s.Seal(42, "sheets", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	second, err := s.Seal(42, "sheets", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if first == second {
		t.Fatal("two seals of the same secret are identical")
	}
}

func TestASealerNeedsAProperKey(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := connector.NewSealer(bytes.Repeat([]byte{1}, size)); !errors.Is(
			err, connector.ErrKeySize,
		) {
			t.Errorf("a %d-byte key was accepted: %v", size, err)
		}
	}
}

func TestSealingNeedsAnOrgAndAConnector(t *testing.T) {
	s := newSealer(t)
	if _, err := s.Seal(0, "sheets", []byte("x")); err == nil {
		t.Error("sealed without an org")
	}
	if _, err := s.Seal(42, "", []byte("x")); err == nil {
		t.Error("sealed without a connector")
	}
}
