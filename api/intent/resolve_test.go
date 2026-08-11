package intent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/ezedike-evan/stelfin/internal/pgtest"
	"github.com/ezedike-evan/stelfin/ledger"
)

// testPGPort must differ from every other package's: `go test ./...` runs
// packages in parallel and two Postgres servers cannot share a port.
const testPGPort = 54331

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	db, err := pgtest.Start(testPGPort, ledger.Migrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testPool = db.Pool

	code := m.Run()

	if err := db.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func saveBeneficiary(t *testing.T, owner, label, address string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO beneficiaries (owner_ref, label, address) VALUES ($1, $2, $3)`,
		owner, label, address)
	if err != nil {
		t.Fatalf("save beneficiary %q: %v", label, err)
	}
}

func beneficiaryIntent(text string) *Grounded {
	return &Grounded{DestinationText: text, DestinationKind: DestinationBeneficiary}
}

func TestResolveExactBeneficiary(t *testing.T) {
	owner := t.Name()
	addr := keypair.MustRandom().Address()
	saveBeneficiary(t, owner, "Brother", addr)

	r := NewResolver(testPool)
	// The user typed lowercase; the saved label is capitalised.
	got, err := r.Resolve(context.Background(), owner, beneficiaryIntent("brother"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != addr {
		t.Errorf("address = %s, want %s", got.Address, addr)
	}
	// The confirmation reads back in the user's own saved words.
	if got.Label != "Brother" {
		t.Errorf("label = %q, want the saved %q", got.Label, "Brother")
	}
}

func TestResolveUniqueSubstringBeneficiary(t *testing.T) {
	owner := t.Name()
	addr := keypair.MustRandom().Address()
	saveBeneficiary(t, owner, "Brother Chidi", addr)

	r := NewResolver(testPool)
	got, err := r.Resolve(context.Background(), owner, beneficiaryIntent("brother"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != addr {
		t.Errorf("address = %s, want %s", got.Address, addr)
	}
}

// TestResolveAmbiguousBeneficiaryAsksRatherThanGuesses is the property that
// matters here. A payment to the wrong Stellar account cannot be recalled, so
// two plausible recipients must become a question, never a coin flip.
func TestResolveAmbiguousBeneficiaryAsksRatherThanGuesses(t *testing.T) {
	owner := t.Name()
	saveBeneficiary(t, owner, "Brother Chidi", keypair.MustRandom().Address())
	saveBeneficiary(t, owner, "Brother Emeka", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), owner, beneficiaryIntent("brother"))
	if !errors.Is(err, ErrDestinationAmbiguous) {
		t.Fatalf("error = %v, want ErrDestinationAmbiguous", err)
	}

	// The candidates must come back so the user can be asked a precise
	// question rather than a generic "which one?".
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error %v does not carry candidates", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want both saved recipients", ambiguous.Candidates)
	}
}

// TestExactMatchBeatsAmbiguity: someone with "Bro" and "Brother" saved who
// types "bro" means "Bro".
func TestExactMatchBeatsAmbiguity(t *testing.T) {
	owner := t.Name()
	exact := keypair.MustRandom().Address()
	saveBeneficiary(t, owner, "Bro", exact)
	saveBeneficiary(t, owner, "Brother", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	got, err := r.Resolve(context.Background(), owner, beneficiaryIntent("bro"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != exact {
		t.Errorf("address = %s, want the exact match %s", got.Address, exact)
	}
}

func TestResolveUnknownBeneficiary(t *testing.T) {
	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), t.Name(), beneficiaryIntent("nobody"))
	if !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound", err)
	}
}

// TestBeneficiariesAreScopedToTheirOwner: one user's saved recipients must
// never be reachable from another's message.
func TestBeneficiariesAreScopedToTheirOwner(t *testing.T) {
	saveBeneficiary(t, t.Name()+"/alice", "Brother", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), t.Name()+"/bob", beneficiaryIntent("brother"))
	if !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound: bob must not see alice's recipients", err)
	}
}

func TestResolveRawAddress(t *testing.T) {
	addr := keypair.MustRandom().Address()
	r := NewResolver(testPool)

	got, err := r.Resolve(context.Background(), t.Name(),
		&Grounded{DestinationText: addr, DestinationKind: DestinationAddress})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != addr {
		t.Errorf("address = %s, want %s", got.Address, addr)
	}
}

// TestResolveRejectsCorruptedAddress: strkey carries a checksum, so a
// transposed or truncated character is caught rather than silently addressing
// some other account.
func TestResolveRejectsCorruptedAddress(t *testing.T) {
	valid := keypair.MustRandom().Address()
	corrupted := []string{
		valid[:len(valid)-1] + "X", // last character changed
		valid[:len(valid)-1],       // truncated
		"G" + valid[2:],            // second character dropped
		"not-an-address",
		"",
	}

	r := NewResolver(testPool)
	for _, bad := range corrupted {
		_, err := r.Resolve(context.Background(), t.Name(),
			&Grounded{DestinationText: bad, DestinationKind: DestinationAddress})
		if !errors.Is(err, ErrDestinationInvalid) {
			t.Errorf("Resolve(%q) error = %v, want ErrDestinationInvalid", bad, err)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+2348012345678":      "+2348012345678",
		"+234 801 234 5678":   "+2348012345678",
		"+234-801-234-5678":   "+2348012345678",
		"+234 (801) 234.5678": "+2348012345678",
	} {
		got, err := NormalizePhone(in)
		if err != nil {
			t.Errorf("NormalizePhone(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizePhoneRefusesToAssumeACountry: inferring a country code would
// silently address a different country's subscriber. Asking is far cheaper
// than a misdirected payment.
func TestNormalizePhoneRefusesToAssumeACountry(t *testing.T) {
	for _, in := range []string{
		"08012345678", // Nigerian local form, but which country?
		"8012345678",
		"",
		"+234",                 // too short
		"+2348012345678901234", // too long
		"+234801234567a",
		"++2348012345678",
	} {
		if got, err := NormalizePhone(in); err == nil {
			t.Errorf("NormalizePhone(%q) = %q, want an error", in, got)
		}
	}
}

func TestResolvePhoneToAccount(t *testing.T) {
	ctx := context.Background()
	phone := "+2348012345678"
	addr := keypair.MustRandom().Address()

	store := ledger.New(testPool)
	account, err := store.EnsureAccount(ctx, ledger.AccountUser, phone, "user "+phone)
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO stellar_accounts (address, ledger_account_id) VALUES ($1, $2)`,
		addr, int64(account)); err != nil {
		t.Fatalf("track address: %v", err)
	}

	r := NewResolver(testPool)
	got, err := r.Resolve(ctx, t.Name(),
		&Grounded{DestinationText: "+234 801 234 5678", DestinationKind: DestinationPhone})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != addr {
		t.Errorf("address = %s, want %s", got.Address, addr)
	}
}

func TestResolvePhoneWithoutAccount(t *testing.T) {
	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), t.Name(),
		&Grounded{DestinationText: "+2349999999999", DestinationKind: DestinationPhone})
	if !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound", err)
	}
}
