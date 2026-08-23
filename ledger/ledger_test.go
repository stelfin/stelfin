package ledger

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/internal/pgtest"
)

// testPGPort is this package's own Postgres port. `go test ./...` runs packages
// in parallel, so every package that needs a database must claim a distinct one.
const testPGPort = 54329

var testPool *pgxpool.Pool

// TestMain runs the whole package against a real Postgres. The invariants under
// test are database constraints, so a fake driver would verify nothing.
func TestMain(m *testing.M) {
	db, err := pgtest.Start(testPGPort, Migrate)
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

// newOrg inserts an org for one test.
//
// Orgs are created by the store layer in production; a test needs one before
// that layer exists, and inserting the row directly is honest about it. The
// slug is derived from the test name so a failure names the test that caused
// it, and hashed because the schema constrains the shape.
func newOrg(t *testing.T) OrgID {
	t.Helper()
	h := fnv.New64a()
	_, _ = h.Write([]byte(t.Name()))
	slug := fmt.Sprintf("t-%016x", h.Sum64())

	var id OrgID
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO orgs (kind, slug, display_name, network)
		VALUES ('dao', $1, $2, 'testnet')
		ON CONFLICT (lower(slug)) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`,
		slug, t.Name(),
	).Scan(&id)
	must(t, err, "create org")
	return id
}

// fixture is an isolated set of accounts and assets for one test.
//
// Every test gets its own org, not just its own member account. Isolation is
// the immediate reason; the useful side effect is that the whole suite runs
// against a database with many tenants in it, so a query that forgot its org
// predicate has somewhere to go wrong.
type fixture struct {
	store    *Store
	org      OrgID
	usdc     AssetID
	xlm      AssetID
	member   AccountID
	external AccountID
	treasury AccountID
	fees     AccountID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s := New(testPool)
	org := newOrg(t)

	// Assets are global: two orgs holding USDC share one row, because the asset
	// on chain is the same asset.
	usdc, err := s.EnsureAsset(ctx, "USDC", "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	must(t, err, "ensure USDC")
	xlm, err := s.EnsureAsset(ctx, "XLM", "")
	must(t, err, "ensure XLM")

	member, err := s.EnsureAccount(ctx, org, AccountMember, t.Name(), "member "+t.Name())
	must(t, err, "ensure member account")
	external, err := s.EnsureAccount(ctx, org, AccountExternal, "", "external")
	must(t, err, "ensure external account")
	treasury, err := s.EnsureAccount(ctx, org, AccountTreasury, "", "treasury")
	must(t, err, "ensure treasury account")
	fees, err := s.EnsureAccount(ctx, org, AccountFeeExpense, "", "fees")
	must(t, err, "ensure fee account")

	return &fixture{s, org, usdc, xlm, member, external, treasury, fees}
}

// deposit credits the user from the external account: the canonical way value
// enters the system.
func (f *fixture) deposit(t *testing.T, key string, amount money.Stroops) TxID {
	t.Helper()
	id, err := f.store.Post(context.Background(), PostRequest{Org: f.org,
		IdempotencyKey: key,
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: amount},
			{Account: f.external, Asset: f.usdc, Amount: -amount},
		},
	})
	must(t, err, "deposit")
	return id
}

func TestPostBalancedTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.deposit(t, t.Name()+"/deposit", money.MustParse("100"))

	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	if want := money.MustParse("100"); got != want {
		t.Errorf("member balance = %s, want %s", got, want)
	}

	// The external account mirrors everything held internally, so it must be
	// exactly the negative of what entered.
	ext, err := f.store.Balance(ctx, f.org, f.external, f.usdc)
	must(t, err, "external balance")
	if want := money.MustParse("-100"); ext != want {
		t.Errorf("external balance = %s, want %s", ext, want)
	}
}

func TestBalanceOfUntouchedAccountIsZero(t *testing.T) {
	f := newFixture(t)
	got, err := f.store.Balance(context.Background(), f.org, f.member, f.usdc)
	must(t, err, "balance")
	if !got.IsZero() {
		t.Errorf("fresh account balance = %s, want 0", got)
	}
}

// TestUnbalancedIsRejectedByTheDatabase is the load-bearing test of the whole
// schema. It bypasses Post's Go-side validation entirely and writes rows
// directly, proving the zero-sum invariant survives code that does not go
// through this package.
func TestUnbalancedIsRejectedByTheDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	must(t, err, "begin")
	defer func() { _ = tx.Rollback(ctx) }()

	var txID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions
			(org_id, idempotency_key, request_fingerprint, kind, occurred_at)
		VALUES ($1, $2, $3, 'deposit', now())
		RETURNING id`,
		int64(f.org), t.Name(), make([]byte, 32),
	).Scan(&txID)
	must(t, err, "insert transaction")

	// One-sided: 100 in, nothing out.
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (transaction_id, account_id, asset_id, amount)
		VALUES ($1, $2, $3, $4)`,
		txID, int64(f.member), int16(f.usdc), int64(money.MustParse("100")),
	)
	must(t, err, "insert one-sided entry")

	// The constraint is deferred, so it fires here and nowhere earlier.
	err = tx.Commit(ctx)
	if err == nil {
		t.Fatal("commit of a one-sided transaction succeeded; the zero-sum constraint is not enforced")
	}
	if classified := classify(err); !errors.Is(classified, ErrUnbalanced) {
		t.Fatalf("commit error = %v, want ErrUnbalanced", classified)
	}
}

func TestUnbalancedIsRejectedByGo(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Post(context.Background(), PostRequest{Org: f.org,
		IdempotencyKey: t.Name(),
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: money.MustParse("100")},
			{Account: f.external, Asset: f.usdc, Amount: money.MustParse("-99")},
		},
	})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post error = %v, want ErrUnbalanced", err)
	}
}

// TestMultiAssetBalancesPerAsset proves the invariant is per-asset. This set
// sums to zero overall only by coincidence across assets; each asset must
// balance on its own.
func TestMultiAssetBalancesPerAsset(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Post(context.Background(), PostRequest{Org: f.org,
		IdempotencyKey: t.Name(),
		Kind:           TxSend,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			// USDC leg balances; XLM leg does not.
			{Account: f.member, Asset: f.usdc, Amount: money.MustParse("10")},
			{Account: f.external, Asset: f.usdc, Amount: money.MustParse("-10")},
			{Account: f.treasury, Asset: f.xlm, Amount: money.MustParse("5")},
		},
	})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post error = %v, want ErrUnbalanced (XLM leg is one-sided)", err)
	}
}

func TestMultiAssetTransactionSucceeds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.store.Post(ctx, PostRequest{Org: f.org,
		IdempotencyKey: t.Name(),
		Kind:           TxSend,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: money.MustParse("10")},
			{Account: f.external, Asset: f.usdc, Amount: money.MustParse("-10")},
			// Fee paid in XLM by the treasury, a separate balanced group.
			{Account: f.fees, Asset: f.xlm, Amount: money.MustParse("0.00001")},
			{Account: f.external, Asset: f.xlm, Amount: money.MustParse("-0.00001")},
		},
	})
	must(t, err, "multi-asset post")

	usdc, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "usdc balance")
	if want := money.MustParse("10"); usdc != want {
		t.Errorf("member USDC = %s, want %s", usdc, want)
	}
	fees, err := f.store.Balance(ctx, f.org, f.fees, f.xlm)
	must(t, err, "fee balance")
	if want := money.MustParse("0.00001"); fees != want {
		t.Errorf("fee XLM = %s, want %s", fees, want)
	}
}

func TestReplayWithSameContentReturnsOriginal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := t.Name()

	first := f.deposit(t, key, money.MustParse("100"))
	second := f.deposit(t, key, money.MustParse("100"))

	if first != second {
		t.Fatalf("replay returned transaction %d, want the original %d", second, first)
	}

	// The decisive check: a replay must not move money twice.
	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	if want := money.MustParse("100"); got != want {
		t.Errorf("balance after replay = %s, want %s (double-posted)", got, want)
	}
}

func TestReplayWithDifferentContentIsRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := t.Name()

	f.deposit(t, key, money.MustParse("100"))

	_, err := f.store.Post(ctx, PostRequest{Org: f.org,
		IdempotencyKey: key,
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: money.MustParse("999")},
			{Account: f.external, Asset: f.usdc, Amount: money.MustParse("-999")},
		},
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("Post error = %v, want ErrIdempotencyKeyReused", err)
	}

	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	if want := money.MustParse("100"); got != want {
		t.Errorf("balance = %s, want %s (the rejected replay must not post)", got, want)
	}
}

// TestFingerprintIgnoresPostingOrder: the same transaction described with its
// lines in a different order is the same transaction, not a conflicting replay.
func TestFingerprintIgnoresPostingOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := t.Name()
	amount := money.MustParse("42")

	first, err := f.store.Post(ctx, PostRequest{Org: f.org,
		IdempotencyKey: key, Kind: TxDeposit, OccurredAt: time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: amount},
			{Account: f.external, Asset: f.usdc, Amount: -amount},
		},
	})
	must(t, err, "first post")

	second, err := f.store.Post(ctx, PostRequest{Org: f.org,
		IdempotencyKey: key, Kind: TxDeposit, OccurredAt: time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.external, Asset: f.usdc, Amount: -amount},
			{Account: f.member, Asset: f.usdc, Amount: amount},
		},
	})
	must(t, err, "reordered replay")

	if first != second {
		t.Fatalf("reordered replay returned %d, want the original %d", second, first)
	}
}

func TestMemberAccountCannotGoNegative(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.deposit(t, t.Name()+"/deposit", money.MustParse("10"))

	_, err := f.store.Post(ctx, PostRequest{Org: f.org,
		IdempotencyKey: t.Name() + "/overspend",
		Kind:           TxWithdrawal,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: money.MustParse("-11")},
			{Account: f.external, Asset: f.usdc, Amount: money.MustParse("11")},
		},
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overspend error = %v, want ErrInsufficientFunds", err)
	}

	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	if want := money.MustParse("10"); got != want {
		t.Errorf("balance after rejected overspend = %s, want %s", got, want)
	}
}

func TestExternalAccountMayGoNegative(t *testing.T) {
	f := newFixture(t)
	// deposit drives external negative; if that were forbidden nothing could
	// ever enter the system.
	f.deposit(t, t.Name(), money.MustParse("1000"))

	got, err := f.store.Balance(context.Background(), f.org, f.external, f.usdc)
	must(t, err, "balance")
	if got.Sign() != -1 {
		t.Errorf("external balance = %s, want negative", got)
	}
}

func TestEntriesAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.deposit(t, t.Name(), money.MustParse("5"))

	if _, err := testPool.Exec(ctx,
		`UPDATE ledger_entries SET amount = 999 WHERE account_id = $1`, int64(f.member),
	); err == nil {
		t.Error("UPDATE on ledger_entries succeeded; history must be immutable")
	}
	if _, err := testPool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE account_id = $1`, int64(f.member),
	); err == nil {
		t.Error("DELETE on ledger_entries succeeded; history must be immutable")
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE ledger_transactions SET kind = 'send'
		  WHERE org_id = $1 AND idempotency_key = $2`, int64(f.org), t.Name(),
	); err == nil {
		t.Error("UPDATE on ledger_transactions succeeded; history must be immutable")
	}
}

func TestZeroAmountPostingIsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Post(context.Background(), PostRequest{Org: f.org,
		IdempotencyKey: t.Name(),
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: f.member, Asset: f.usdc, Amount: 0},
			{Account: f.external, Asset: f.usdc, Amount: 0},
		},
	})
	if !errors.Is(err, ErrZeroAmount) {
		t.Fatalf("Post error = %v, want ErrZeroAmount", err)
	}
}

func TestEmptyPostingsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Post(context.Background(), PostRequest{Org: f.org,
		IdempotencyKey: t.Name(),
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
	})
	if !errors.Is(err, ErrNoPostings) {
		t.Fatalf("Post error = %v, want ErrNoPostings", err)
	}
}

// TestBalancesReconcileWithEntries is the property test that catches the
// classic ledger bug: a derived balance table that silently drifts from the
// entries it summarises. It posts a randomised sequence and then checks every
// balance row against a fresh SUM over history.
func TestBalancesReconcileWithEntries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(20260807))

	// Seed enough to keep the member solvent through the withdrawals below.
	f.deposit(t, t.Name()+"/seed", money.MustParse("1000000"))

	for i := 0; i < 200; i++ {
		// Bounded so the member never goes negative, which is a separate
		// invariant with its own test.
		amount := money.Stroops(rng.Int63n(int64(money.MustParse("100"))) + 1)
		dir := money.Stroops(1)
		if rng.Intn(2) == 0 {
			dir = -1
		}
		signed := amount * dir

		_, err := f.store.Post(ctx, PostRequest{Org: f.org,
			IdempotencyKey: fmt.Sprintf("%s/%d", t.Name(), i),
			Kind:           TxSend,
			OccurredAt:     time.Unix(1700000000+int64(i), 0),
			Postings: []Posting{
				{Account: f.member, Asset: f.usdc, Amount: signed},
				{Account: f.external, Asset: f.usdc, Amount: -signed},
			},
		})
		must(t, err, fmt.Sprintf("post %d", i))
	}

	rows, err := testPool.Query(ctx, `
		SELECT b.account_id, b.asset_id, b.balance,
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e
		                  WHERE e.account_id = b.account_id AND e.asset_id = b.asset_id), 0)
		  FROM ledger_balances b`)
	must(t, err, "query balances")
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var account, cached, summed int64
		var asset int16
		must(t, rows.Scan(&account, &asset, &cached, &summed), "scan")
		if cached != summed {
			t.Errorf("account %d asset %d: ledger_balances says %d, SUM(entries) says %d",
				account, asset, cached, summed)
		}
		checked++
	}
	must(t, rows.Err(), "iterate balances")
	if checked == 0 {
		t.Fatal("no balance rows were checked; the reconciliation proved nothing")
	}
}

// TestConcurrentPostsStayConsistent runs many posters at once against one
// account. Whatever interleaving Postgres chooses, the derived balance must
// still equal the sum of entries.
func TestConcurrentPostsStayConsistent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const workers, perWorker = 8, 25
	amount := money.MustParse("1")

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, err := f.store.Post(ctx, PostRequest{Org: f.org,
					IdempotencyKey: fmt.Sprintf("%s/%d/%d", t.Name(), w, i),
					Kind:           TxDeposit,
					OccurredAt:     time.Unix(1700000000, 0),
					Postings: []Posting{
						{Account: f.member, Asset: f.usdc, Amount: amount},
						{Account: f.external, Asset: f.usdc, Amount: -amount},
					},
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent post failed: %v", err)
	}

	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	want := amount * workers * perWorker
	if got != want {
		t.Errorf("balance after %d concurrent posts = %s, want %s", workers*perWorker, got, want)
	}

	var summed int64
	must(t, testPool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM ledger_entries WHERE account_id = $1 AND asset_id = $2`,
		int64(f.member), int16(f.usdc),
	).Scan(&summed), "sum entries")
	if money.Stroops(summed) != got {
		t.Errorf("cached balance %s disagrees with SUM(entries) %s", got, money.Stroops(summed))
	}
}

// TestConcurrentReplayPostsOnce hammers one idempotency key from many
// goroutines. Exactly one transaction must exist afterwards.
func TestConcurrentReplayPostsOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	amount := money.MustParse("7")

	const workers = 16
	ids := make(chan TxID, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := f.store.Post(ctx, PostRequest{Org: f.org,
				IdempotencyKey: t.Name(),
				Kind:           TxDeposit,
				OccurredAt:     time.Unix(1700000000, 0),
				Postings: []Posting{
					{Account: f.member, Asset: f.usdc, Amount: amount},
					{Account: f.external, Asset: f.usdc, Amount: -amount},
				},
			})
			if err != nil {
				t.Errorf("concurrent replay: %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[TxID]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Errorf("concurrent replay produced %d distinct transactions, want 1: %v", len(seen), seen)
	}

	got, err := f.store.Balance(ctx, f.org, f.member, f.usdc)
	must(t, err, "balance")
	if got != amount {
		t.Errorf("balance = %s, want %s (money moved more than once)", got, amount)
	}
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestCrossOrgTransactionIsRejectedByTheDatabase is the tenancy invariant, and
// like the zero-sum one it is proved by bypassing this package entirely.
//
// One DAO's money moving into another DAO's books is the failure multi-tenancy
// exists to prevent, and it is exactly the failure a forgotten predicate in one
// query out of forty would cause. So it is a constraint, not a convention.
func TestCrossOrgTransactionIsRejectedByTheDatabase(t *testing.T) {
	a := newFixture(t)
	ctx := context.Background()

	// A second org with its own accounts, standing in for another tenant.
	otherOrg := newOrgNamed(t, "t-other-crossorg")

	victim, err := a.store.EnsureAccount(ctx, otherOrg, AccountMember, "victim", "victim")
	must(t, err, "ensure the other org's member account")

	tx, err := testPool.Begin(ctx)
	must(t, err, "begin")
	defer func() { _ = tx.Rollback(ctx) }()

	var txID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions
			(org_id, idempotency_key, request_fingerprint, kind, occurred_at)
		VALUES ($1, $2, $3, 'send', now())
		RETURNING id`,
		int64(a.org), t.Name(), make([]byte, 32),
	).Scan(&txID)
	must(t, err, "insert transaction")

	// Balanced, and reaching across tenants: this org's external account funds
	// another org's member.
	amount := int64(money.MustParse("100"))
	for _, e := range []struct {
		account AccountID
		amount  int64
	}{
		{victim, amount},
		{a.external, -amount},
	} {
		_, err = tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, asset_id, amount)
			VALUES ($1, $2, $3, $4)`,
			txID, int64(e.account), int16(a.usdc), e.amount)
		must(t, err, "insert entry")
	}

	err = tx.Commit(ctx)
	if err == nil {
		t.Fatal("a transaction spanning two orgs committed; tenants are not isolated")
	}
	if classified := classify(err); !errors.Is(classified, ErrCrossOrg) {
		t.Fatalf("commit error = %v, want ErrCrossOrg", classified)
	}
}

// TestBalanceIsScopedToItsOrg: an account id leaked from another tenant must
// read as zero rather than as their balance.
func TestBalanceIsScopedToItsOrg(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.deposit(t, t.Name(), money.MustParse("500"))

	stranger := newOrgNamed(t, "t-stranger-balance")

	got, err := f.store.Balance(ctx, stranger, f.member, f.usdc)
	must(t, err, "balance")
	if !got.IsZero() {
		t.Fatalf("another org read %s from an account that is not theirs", got)
	}
}

// TestIdempotencyKeysAreScopedToTheirOrg: two DAOs ingesting the same on-chain
// operation each record it in their own books, and a global key would let the
// first to arrive silence the second.
func TestIdempotencyKeysAreScopedToTheirOrg(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	other := newOrgNamed(t, "t-shared-key")
	otherMember, err := f.store.EnsureAccount(ctx, other, AccountMember, "m", "member")
	must(t, err, "ensure member")
	otherExternal, err := f.store.EnsureAccount(ctx, other, AccountExternal, "", "external")
	must(t, err, "ensure external")

	const sharedKey = "horizon:op:12345"
	amount := money.MustParse("3")

	first := f.deposit(t, sharedKey, amount)

	second, err := f.store.Post(ctx, PostRequest{
		Org:            other,
		IdempotencyKey: sharedKey,
		Kind:           TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []Posting{
			{Account: otherMember, Asset: f.usdc, Amount: amount},
			{Account: otherExternal, Asset: f.usdc, Amount: -amount},
		},
	})
	must(t, err, "the same key in another org")

	if first == second {
		t.Fatal("the same idempotency key in two orgs returned one transaction")
	}
}

// newOrgNamed creates an org with a fixed slug, for tests that need a second
// tenant. Idempotent across runs against the same database.
func newOrgNamed(t *testing.T, slug string) OrgID {
	t.Helper()
	ctx := context.Background()
	var id OrgID
	err := testPool.QueryRow(ctx, `
		INSERT INTO orgs (kind, slug, display_name, network)
		VALUES ('dao', $1, $1, 'testnet')
		ON CONFLICT (lower(slug)) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`, slug).Scan(&id)
	must(t, err, "create org "+slug)
	return id
}
