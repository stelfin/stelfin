package intent

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
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

// testOrg creates an org for one test, so beneficiaries saved by one test are
// unreachable from another — which is also the property being tested.
func testOrg(t *testing.T) ledger.OrgID {
	t.Helper()
	h := fnv.New64a()
	_, _ = h.Write([]byte(t.Name()))

	var id ledger.OrgID
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO orgs (kind, slug, display_name, network)
		VALUES ('dao', $1, $2, 'testnet')
		ON CONFLICT (lower(slug)) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`,
		fmt.Sprintf("t-%016x", h.Sum64()), t.Name(),
	).Scan(&id)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return id
}

func saveBeneficiary(t *testing.T, org ledger.OrgID, owner, label, address string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO beneficiaries (org_id, owner_ref, label, address) VALUES ($1, $2, $3, $4)`,
		int64(org), owner, label, address)
	if err != nil {
		t.Fatalf("save beneficiary %q: %v", label, err)
	}
}

func beneficiaryIntent(text string) *Grounded {
	return &Grounded{DestinationText: text, DestinationKind: DestinationBeneficiary}
}

func TestResolveExactBeneficiary(t *testing.T) {
	org := testOrg(t)
	owner := t.Name()
	addr := keypair.MustRandom().Address()
	saveBeneficiary(t, org, owner, "Brother", addr)

	r := NewResolver(testPool)
	// The user typed lowercase; the saved label is capitalised.
	got, err := r.Resolve(context.Background(), org, owner, beneficiaryIntent("brother"))
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
	org := testOrg(t)
	owner := t.Name()
	addr := keypair.MustRandom().Address()
	saveBeneficiary(t, org, owner, "Brother Chidi", addr)

	r := NewResolver(testPool)
	got, err := r.Resolve(context.Background(), org, owner, beneficiaryIntent("brother"))
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
	org := testOrg(t)
	owner := t.Name()
	saveBeneficiary(t, org, owner, "Brother Chidi", keypair.MustRandom().Address())
	saveBeneficiary(t, org, owner, "Brother Emeka", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), org, owner, beneficiaryIntent("brother"))
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
	org := testOrg(t)
	owner := t.Name()
	exact := keypair.MustRandom().Address()
	saveBeneficiary(t, org, owner, "Bro", exact)
	saveBeneficiary(t, org, owner, "Brother", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	got, err := r.Resolve(context.Background(), org, owner, beneficiaryIntent("bro"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Address != exact {
		t.Errorf("address = %s, want the exact match %s", got.Address, exact)
	}
}

func TestResolveUnknownBeneficiary(t *testing.T) {
	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), testOrg(t), t.Name(), beneficiaryIntent("nobody"))
	if !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound", err)
	}
}

// TestBeneficiariesAreScopedToTheirOwner: one member's saved recipients must
// never be reachable from another's message.
func TestBeneficiariesAreScopedToTheirOwner(t *testing.T) {
	org := testOrg(t)
	saveBeneficiary(t, org, t.Name()+"/alice", "Brother", keypair.MustRandom().Address())

	r := NewResolver(testPool)
	_, err := r.Resolve(context.Background(), org, t.Name()+"/bob", beneficiaryIntent("brother"))
	if !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound: bob must not see alice's recipients", err)
	}
}

// TestBeneficiariesAreScopedToTheirOrg: the same owner reference in a different
// org is a different person entirely, and their address book must not follow
// them across tenants.
func TestBeneficiariesAreScopedToTheirOrg(t *testing.T) {
	owner := t.Name()
	mine := testOrg(t)
	saveBeneficiary(t, mine, owner, "Payroll", keypair.MustRandom().Address())

	var theirs ledger.OrgID
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO orgs (kind, slug, display_name, network)
		VALUES ('dao', 't-other-tenant', 'other', 'testnet')
		ON CONFLICT (lower(slug)) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`).Scan(&theirs)
	if err != nil {
		t.Fatalf("create the other org: %v", err)
	}

	r := NewResolver(testPool)
	if _, err := r.Resolve(context.Background(), theirs, owner, beneficiaryIntent("payroll")); !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("error = %v, want ErrDestinationNotFound: another org read this one's address book", err)
	}
}

func TestResolveRawAddress(t *testing.T) {
	addr := keypair.MustRandom().Address()
	r := NewResolver(testPool)

	got, err := r.Resolve(context.Background(), testOrg(t), t.Name(),
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
		_, err := r.Resolve(context.Background(), testOrg(t), t.Name(),
			&Grounded{DestinationText: bad, DestinationKind: DestinationAddress})
		if !errors.Is(err, ErrDestinationInvalid) {
			t.Errorf("Resolve(%q) error = %v, want ErrDestinationInvalid", bad, err)
		}
	}
}

// TestResolveRejectsAnUnknownDestinationKind: the model proposes the kind, and
// an unrecognised one must be refused rather than falling through to a default
// resolver. "phone" in particular used to be a kind and is not one any more —
// a decoder that still emitted it must fail loudly, not quietly resolve as
// something else.
func TestResolveRejectsAnUnknownDestinationKind(t *testing.T) {
	r := NewResolver(testPool)
	for _, kind := range []DestinationKind{"phone", "", "handle", "email"} {
		_, err := r.Resolve(context.Background(), testOrg(t), t.Name(),
			&Grounded{DestinationText: "+2348012345678", DestinationKind: kind})
		if err == nil {
			t.Errorf("destination kind %q was accepted", kind)
		}
	}
}
