package ingestion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
)

// testPGPort must differ from every other package's: `go test ./...` runs
// packages in parallel and two Postgres servers cannot share a port.
const testPGPort = 54330

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

// fakeHorizon serves canned pages keyed by the cursor requested.
type fakeHorizon struct {
	pages map[string]operations.OperationsPage
	err   error

	requested []string
}

func (f *fakeHorizon) Payments(req horizonclient.OperationRequest) (operations.OperationsPage, error) {
	f.requested = append(f.requested, req.Cursor)
	if f.err != nil {
		return operations.OperationsPage{}, f.err
	}
	return f.pages[req.Cursor], nil
}

func page(records ...operations.Operation) operations.OperationsPage {
	var p operations.OperationsPage
	p.Embedded.Records = records
	return p
}

var testIssuer = keypair.MustRandom().Address()

// payment builds a successful USDC payment operation. The paging token doubles
// as the operation id so cursor assertions read clearly.
func payment(id, from, to, amount string) operations.Payment {
	p := operations.Payment{From: from, To: to, Amount: amount}
	p.ID = id
	p.PT = id
	p.TransactionSuccessful = true
	p.TransactionHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	p.LedgerCloseTime = time.Unix(1700000000, 0)
	p.Asset = base.Asset{Type: "credit_alphanum4", Code: "USDC", Issuer: testIssuer}
	return p
}

type fixture struct {
	ing      *Ingester
	store    *ledger.Store
	horizon  *fakeHorizon
	usdc     ledger.AssetID
	user     ledger.AccountID
	userAddr string
	external ledger.AccountID
}

func newFixture(t *testing.T, pages map[string]operations.OperationsPage) *fixture {
	t.Helper()
	ctx := context.Background()

	store := ledger.New(testPool)
	usdc, err := store.EnsureAsset(ctx, "USDC", testIssuer)
	must(t, err, "ensure USDC")

	user, err := store.EnsureAccount(ctx, ledger.AccountUser, t.Name(), "user "+t.Name())
	must(t, err, "ensure user")
	external, err := store.EnsureAccount(ctx, ledger.AccountExternal, "", "external")
	must(t, err, "ensure external")

	h := &fakeHorizon{pages: pages}
	// Stream is per-test so each gets an independent cursor.
	ing, err := New(ctx, h, store, testPool, Config{Stream: t.Name(), PageSize: 200})
	must(t, err, "new ingester")

	addr := keypair.MustRandom().Address()
	must(t, ing.Track(ctx, addr, user), "track user address")

	return &fixture{ing: ing, store: store, horizon: h, usdc: usdc,
		user: user, userAddr: addr, external: external}
}

func (f *fixture) balance(t *testing.T) money.Stroops {
	t.Helper()
	b, err := f.store.Balance(context.Background(), f.user, f.usdc)
	must(t, err, "balance")
	return b
}

func TestIngestDeposit(t *testing.T) {
	f := newFixture(t, nil)
	stranger := keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"": page(payment("op1", stranger, f.userAddr, "100.0000000")),
	}

	n, err := f.ing.Once(context.Background())
	must(t, err, "ingest")
	if n != 1 {
		t.Fatalf("consumed %d records, want 1", n)
	}
	if got, want := f.balance(t), money.MustParse("100"); got != want {
		t.Errorf("user balance = %s, want %s", got, want)
	}

	ext, err := f.store.Balance(context.Background(), f.external, f.usdc)
	must(t, err, "external balance")
	if ext.Sign() != -1 {
		t.Errorf("external balance = %s, want negative: value entered the system", ext)
	}
}

func TestIngestWithdrawal(t *testing.T) {
	f := newFixture(t, nil)
	stranger := keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"":     page(payment("w-in", stranger, f.userAddr, "50.0000000")),
		"w-in": page(payment("w-out", f.userAddr, stranger, "20.0000000")),
	}

	_, err := f.ing.Once(context.Background())
	must(t, err, "ingest deposit")
	_, err = f.ing.Once(context.Background())
	must(t, err, "ingest withdrawal")

	if got, want := f.balance(t), money.MustParse("30"); got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}
}

// TestIngestInternalSend: when both sides are ours, nothing crosses the system
// boundary and the external account must not move.
func TestIngestInternalSend(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	other, err := f.store.EnsureAccount(ctx, ledger.AccountUser, t.Name()+"/other", "other")
	must(t, err, "ensure other user")
	otherAddr := keypair.MustRandom().Address()
	must(t, f.ing.Track(ctx, otherAddr, other), "track other")

	stranger := keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"":     page(payment("s-in", stranger, f.userAddr, "80.0000000")),
		"s-in": page(payment("s-mv", f.userAddr, otherAddr, "30.0000000")),
	}

	extBefore, err := f.store.Balance(ctx, f.external, f.usdc)
	must(t, err, "external before")

	_, err = f.ing.Once(ctx)
	must(t, err, "ingest deposit")
	extAfterDeposit, err := f.store.Balance(ctx, f.external, f.usdc)
	must(t, err, "external after deposit")

	_, err = f.ing.Once(ctx)
	must(t, err, "ingest internal send")

	if got, want := f.balance(t), money.MustParse("50"); got != want {
		t.Errorf("sender balance = %s, want %s", got, want)
	}
	otherBal, err := f.store.Balance(ctx, other, f.usdc)
	must(t, err, "recipient balance")
	if want := money.MustParse("30"); otherBal != want {
		t.Errorf("recipient balance = %s, want %s", otherBal, want)
	}

	extAfterSend, err := f.store.Balance(ctx, f.external, f.usdc)
	must(t, err, "external after send")
	if extAfterSend != extAfterDeposit {
		t.Errorf("external moved by %s on an internal transfer; it should not move at all",
			extAfterSend-extAfterDeposit)
	}
	if extAfterDeposit == extBefore {
		t.Error("external did not move on a deposit; value must be seen entering the system")
	}
}

// TestIngestSkipsUntrackedButAdvances: a payment between two strangers is not
// ours to record, but the cursor must still move or the stream wedges on it
// forever.
func TestIngestSkipsUntrackedButAdvances(t *testing.T) {
	f := newFixture(t, nil)
	a, b := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"": page(payment("stranger-op", a, b, "1000.0000000")),
	}

	n, err := f.ing.Once(context.Background())
	must(t, err, "ingest")
	if n != 1 {
		t.Fatalf("consumed %d records, want 1", n)
	}
	if got := f.balance(t); !got.IsZero() {
		t.Errorf("balance = %s, want 0: this payment was not ours", got)
	}
	if cursor := f.cursor(t); cursor != "stranger-op" {
		t.Errorf("cursor = %q, want %q: an unrecognised record must not wedge the stream", cursor, "stranger-op")
	}
}

func TestIngestSkipsFailedTransaction(t *testing.T) {
	f := newFixture(t, nil)
	stranger := keypair.MustRandom().Address()
	failed := payment("failed-op", stranger, f.userAddr, "100.0000000")
	failed.TransactionSuccessful = false
	f.horizon.pages = map[string]operations.OperationsPage{"": page(failed)}

	_, err := f.ing.Once(context.Background())
	must(t, err, "ingest")
	if got := f.balance(t); !got.IsZero() {
		t.Errorf("balance = %s, want 0: a failed transaction moved no money", got)
	}
}

// TestReplayDoesNotDoublePost simulates the crash window that at-least-once
// delivery creates: the ledger write committed but the cursor never advanced,
// so the same page arrives again.
func TestReplayDoesNotDoublePost(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	stranger := keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"":       page(payment("dup-op", stranger, f.userAddr, "100.0000000")),
		"dup-op": {},
	}

	_, err := f.ing.Once(ctx)
	must(t, err, "first ingest")

	// Rewind the cursor by hand: exactly what a crash between the post and the
	// cursor write would leave behind.
	_, err = testPool.Exec(ctx, `UPDATE ingestion_cursors SET cursor = '' WHERE stream = $1`, t.Name())
	must(t, err, "rewind cursor")

	_, err = f.ing.Once(ctx)
	must(t, err, "replayed ingest")

	if got, want := f.balance(t), money.MustParse("100"); got != want {
		t.Errorf("balance = %s, want %s: the replay posted the payment twice", got, want)
	}
}

func TestCursorAdvancesAcrossPages(t *testing.T) {
	f := newFixture(t, nil)
	stranger := keypair.MustRandom().Address()
	f.horizon.pages = map[string]operations.OperationsPage{
		"": page(
			payment("op-a", stranger, f.userAddr, "10.0000000"),
			payment("op-b", stranger, f.userAddr, "20.0000000"),
		),
		"op-b": page(payment("op-c", stranger, f.userAddr, "30.0000000")),
		"op-c": {},
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.ing.Once(ctx); err != nil {
			t.Fatalf("ingest round %d: %v", i, err)
		}
	}

	if got, want := f.balance(t), money.MustParse("60"); got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}
	if cursor := f.cursor(t); cursor != "op-c" {
		t.Errorf("cursor = %q, want %q", cursor, "op-c")
	}
	// Each round must resume where the last stopped, not restart.
	want := []string{"", "op-b", "op-c"}
	if len(f.horizon.requested) != len(want) {
		t.Fatalf("requested cursors %v, want %v", f.horizon.requested, want)
	}
	for i := range want {
		if f.horizon.requested[i] != want[i] {
			t.Errorf("request %d used cursor %q, want %q", i, f.horizon.requested[i], want[i])
		}
	}
}

// TestCursorHoldsAtAFailedRecord is the property that makes at-least-once safe:
// if a record cannot be posted, the cursor must not move past it, or that
// payment is lost silently.
func TestCursorHoldsAtAFailedRecord(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	stranger := keypair.MustRandom().Address()

	// The user has no balance, so a withdrawal cannot post.
	f.horizon.pages = map[string]operations.OperationsPage{
		"": page(
			payment("good-op", stranger, f.userAddr, "5.0000000"),
			payment("bad-op", f.userAddr, stranger, "999.0000000"),
		),
	}

	_, err := f.ing.Once(ctx)
	if err == nil {
		t.Fatal("expected an error: the overdrawing record cannot post")
	}
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("error = %v, want ErrInsufficientFunds", err)
	}
	if cursor := f.cursor(t); cursor != "good-op" {
		t.Errorf("cursor = %q, want %q: it must not advance past the record that failed", cursor, "good-op")
	}
}

func TestIngestNativeAsset(t *testing.T) {
	f := newFixture(t, nil)
	stranger := keypair.MustRandom().Address()
	native := payment("xlm-op", stranger, f.userAddr, "3.5000000")
	native.Asset = base.Asset{Type: "native"}
	f.horizon.pages = map[string]operations.OperationsPage{"": page(native)}

	must(t, mustErr(f.ing.Once(context.Background())), "ingest native")

	xlm, err := f.store.EnsureAsset(context.Background(), "XLM", "")
	must(t, err, "resolve XLM")
	got, err := f.store.Balance(context.Background(), f.user, xlm)
	must(t, err, "xlm balance")
	if want := money.MustParse("3.5"); got != want {
		t.Errorf("XLM balance = %s, want %s", got, want)
	}
}

func TestFetchErrorIsReported(t *testing.T) {
	f := newFixture(t, nil)
	f.horizon.err = errors.New("horizon unavailable")

	if _, err := f.ing.Once(context.Background()); err == nil {
		t.Fatal("expected the fetch error to surface")
	}
}

func (f *fixture) cursor(t *testing.T) string {
	t.Helper()
	var cursor string
	err := testPool.QueryRow(context.Background(),
		`SELECT cursor FROM ingestion_cursors WHERE stream = $1`, t.Name(),
	).Scan(&cursor)
	must(t, err, "read cursor")
	return cursor
}

func mustErr(_ int, err error) error { return err }

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
