package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
)

// testPGPort is this package's own Postgres port. `go test ./...` runs packages
// in parallel, so every package that needs a database must claim a distinct one.
const testPGPort = 54333

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

// tenant is one org with everything a test needs inside it.
type tenant struct {
	org      Org
	member   Member
	account  ledger.AccountID
	external ledger.AccountID
	treasury ledger.AccountID
	address  string
	usdc     ledger.AssetID
}

var slugSeq int

// newTenant creates an org, a member, and the accounts they transact through.
func newTenant(t *testing.T, s *Store, spaceID string) *tenant {
	t.Helper()
	ctx := context.Background()

	slugSeq++
	org, err := s.CreateOrg(ctx, CreateOrgParams{
		Slug:        fmt.Sprintf("t-%d-%d", testPGPort, slugSeq),
		DisplayName: t.Name(),
		Network:     "testnet",
		Channel:     chat.Telegram,
		SpaceID:     spaceID,
		InstalledBy: "telegram:1",
	})
	must(t, err, "create org")

	member, err := s.EnsureMember(ctx, org.ID, chat.Telegram, "u"+spaceID, "ada")
	must(t, err, "ensure member")

	ownerRef := "telegram:u" + spaceID
	account, err := s.EnsureAccount(ctx, org.ID, ledger.AccountMember, ownerRef, ownerRef)
	must(t, err, "ensure member account")

	external, err := s.EnsureAccount(ctx, org.ID, ledger.AccountExternal, "", "external")
	must(t, err, "ensure external")
	treasury, err := s.EnsureAccount(ctx, org.ID, ledger.AccountTreasury, "", "ops")
	must(t, err, "ensure treasury")

	address := keypair.MustRandom().Address()
	must(t, s.TrackAddress(ctx, org.ID, address, account, RoleMember), "track address")

	usdc, err := s.Ledger().EnsureAsset(ctx, "USDC",
		"GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	must(t, err, "ensure USDC")

	return &tenant{org, member, account, external, treasury, address, usdc}
}

// deposit credits the member from the org's external account.
func (tn *tenant) deposit(t *testing.T, s *Store, key string, amount money.Stroops) {
	t.Helper()
	_, err := s.Post(context.Background(), ledger.PostRequest{
		Org:            tn.org.ID,
		IdempotencyKey: key,
		Kind:           ledger.TxDeposit,
		OccurredAt:     time.Unix(1700000000, 0),
		Postings: []ledger.Posting{
			{Account: tn.account, Asset: tn.usdc, Amount: amount},
			{Account: tn.external, Asset: tn.usdc, Amount: -amount},
		},
	})
	must(t, err, "deposit")
}

func TestEnsurePlatformOrgIsASingleton(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	first, err := s.EnsurePlatformOrg(ctx, "testnet")
	must(t, err, "ensure platform org")
	second, err := s.EnsurePlatformOrg(ctx, "testnet")
	must(t, err, "ensure platform org again")

	if first.ID != second.ID {
		t.Fatalf("two platform orgs: %d and %d", first.ID, second.ID)
	}
	if first.Kind != OrgPlatform {
		t.Errorf("kind = %q", first.Kind)
	}
}

// TestPlatformOrgRefusesANetworkChange: coming up pointed at a different
// network than the books were written against is not something to correct
// silently. A testnet org's treasury address means nothing on mainnet.
func TestPlatformOrgRefusesANetworkChange(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	_, err := s.EnsurePlatformOrg(ctx, "testnet")
	must(t, err, "ensure platform org")

	if _, err := s.EnsurePlatformOrg(ctx, "public"); err == nil {
		t.Fatal("switching networks against the same database was allowed")
	}
}

func TestOrgForSpace(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100001")

	got, ok, err := s.OrgForSpace(ctx, chat.Telegram, "-100001")
	must(t, err, "org for space")
	if !ok || got.ID != tn.org.ID {
		t.Fatalf("resolved %+v, want org %d", got, tn.org.ID)
	}
}

// TestOrgForUnregisteredSpaceIsSilence: the bot has been added somewhere nobody
// ran setup. That is an ordinary state, not a failure, and the correct response
// is to say nothing rather than reply to a room that never asked.
func TestOrgForUnregisteredSpaceIsSilence(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	for _, c := range []struct {
		channel chat.Channel
		space   string
	}{
		{chat.Telegram, "-999999999"},
		{chat.Discord, "-100001"}, // same id, different platform
		{"whatsapp", "-100001"},   // a channel this build does not serve
		{chat.Telegram, ""},
	} {
		_, ok, err := s.OrgForSpace(ctx, c.channel, c.space)
		if err != nil {
			t.Errorf("%s/%s: %v", c.channel, c.space, err)
		}
		if ok {
			t.Errorf("%s/%s resolved to an org", c.channel, c.space)
		}
	}
}

// TestASpaceBelongsToOneOrg: (channel, space_id) is the tenant key, so a second
// org claiming the same space must fail rather than create an ambiguity that
// routes messages to whichever row a query happens to return.
func TestASpaceBelongsToOneOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	newTenant(t, s, "-100002")

	slugSeq++
	_, err := s.CreateOrg(ctx, CreateOrgParams{
		Slug:        fmt.Sprintf("t-%d-%d", testPGPort, slugSeq),
		DisplayName: "impostor",
		Network:     "testnet",
		Channel:     chat.Telegram,
		SpaceID:     "-100002",
	})
	if !errors.Is(err, ErrSpaceRegistered) {
		t.Fatalf("error = %v, want ErrSpaceRegistered", err)
	}

	// The rejected org must not survive: a tenant nothing routes to is worse
	// than no tenant.
	var orgs int
	must(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM orgs WHERE display_name = 'impostor'`).Scan(&orgs), "count")
	if orgs != 0 {
		t.Errorf("the rejected org was left behind (%d rows)", orgs)
	}
}

func TestCreateOrgRejectsBadInput(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()

	base := CreateOrgParams{
		Slug: "valid-slug", DisplayName: "ok", Network: "testnet",
		Channel: chat.Telegram, SpaceID: "-1",
	}
	for name, mutate := range map[string]func(*CreateOrgParams){
		"empty slug":      func(p *CreateOrgParams) { p.Slug = "" },
		"slug with space": func(p *CreateOrgParams) { p.Slug = "not a slug" },
		"slug uppercase":  func(p *CreateOrgParams) { p.Slug = "NotASlug!" },
		"no display name": func(p *CreateOrgParams) { p.DisplayName = "  " },
		"bad channel":     func(p *CreateOrgParams) { p.Channel = "whatsapp" },
		"no space":        func(p *CreateOrgParams) { p.SpaceID = "" },
		"bad network":     func(p *CreateOrgParams) { p.Network = "futurenet" },
	} {
		p := base
		mutate(&p)
		if _, err := s.CreateOrg(ctx, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEnsureMemberIsIdempotent(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100003")

	again, err := s.EnsureMember(ctx, tn.org.ID, chat.Telegram, "u-100003", "ada-renamed")
	must(t, err, "ensure member again")
	if again.ID != tn.member.ID {
		t.Fatalf("a second message created member %d, want %d", again.ID, tn.member.ID)
	}
}

// TestOneHumanManyHandles is the identity model: the same person on Telegram
// and Discord is one member with one wallet, not two members with two.
func TestOneHumanManyHandles(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100004")

	must(t, s.LinkIdentity(ctx, tn.org.ID, tn.member.ID, chat.Discord, "d-42", "ada", tn.member.ID),
		"link discord identity")

	viaDiscord, ok, err := s.MemberByIdentity(ctx, tn.org.ID, chat.Discord, "d-42")
	must(t, err, "member by discord identity")
	if !ok || viaDiscord.ID != tn.member.ID {
		t.Fatalf("discord resolved to member %+v, want %d", viaDiscord, tn.member.ID)
	}
}

func TestSetMemberAddress(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100005")

	address := keypair.MustRandom().Address()
	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID, address, AddressLinked), "set address")

	got, err := s.Member(ctx, tn.org.ID, tn.member.ID)
	must(t, err, "read member")
	if got.Address != address {
		t.Errorf("address = %q, want %q", got.Address, address)
	}
	// The schema requires the proof alongside the address, so a verified
	// timestamp must be present rather than left at its zero value.
	if got.VerifiedAt.IsZero() || got.VerifiedAt.Year() < 2000 {
		t.Errorf("verified_at = %v; an address without its proof is indistinguishable "+
			"from a verified one", got.VerifiedAt)
	}
	if got.AddressSource != AddressLinked {
		t.Errorf("source = %q", got.AddressSource)
	}
}

func TestTwoMembersCannotShareAnAddress(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100006")

	other, err := s.EnsureMember(ctx, tn.org.ID, chat.Telegram, "u-other", "bob")
	must(t, err, "ensure second member")

	address := keypair.MustRandom().Address()
	must(t, s.SetMemberAddress(ctx, tn.org.ID, tn.member.ID, address, AddressLinked), "first")

	if err := s.SetMemberAddress(ctx, tn.org.ID, other.ID, address, AddressLinked); !errors.Is(err, ErrAddressTaken) {
		t.Fatalf("error = %v, want ErrAddressTaken — two members sharing an address "+
			"makes 'who approved this' unanswerable", err)
	}
}

// TestTheSameWalletCanJoinTwoOrgs: the address constraint is per-org on
// purpose. A person legitimately belongs to several DAOs with one wallet, and a
// global constraint would let the first org they joined lock them out of the
// second.
func TestTheSameWalletCanJoinTwoOrgs(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "-100007")
	b := newTenant(t, s, "-100008")

	address := keypair.MustRandom().Address()
	must(t, s.SetMemberAddress(ctx, a.org.ID, a.member.ID, address, AddressLinked), "org a")
	must(t, s.SetMemberAddress(ctx, b.org.ID, b.member.ID, address, AddressLinked), "org b")
}

func TestEffectiveRole(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100009")

	got, err := s.EffectiveRole(ctx, tn.org.ID, tn.member.ID, chat.Telegram, nil)
	must(t, err, "effective role")
	if got != chat.RoleNone {
		t.Errorf("a member with no roles has %s, want none", got)
	}

	must(t, s.GrantRole(ctx, tn.org.ID, tn.member.ID, chat.RoleProposer, 0), "grant proposer")
	must(t, s.BindRole(ctx, tn.org.ID, chat.Telegram, "administrator", chat.RoleApprover), "bind")

	// The highest of the two applies.
	got, err = s.EffectiveRole(ctx, tn.org.ID, tn.member.ID, chat.Telegram, []string{"administrator"})
	must(t, err, "effective role with a platform role")
	if got != chat.RoleApprover {
		t.Errorf("effective role = %s, want approver", got)
	}

	// A platform role they do not hold confers nothing.
	got, err = s.EffectiveRole(ctx, tn.org.ID, tn.member.ID, chat.Telegram, []string{"member"})
	must(t, err, "effective role without the platform role")
	if got != chat.RoleProposer {
		t.Errorf("effective role = %s, want proposer", got)
	}
}

// TestAPlatformRoleCannotConferAdmin: a guild administrator can create a role
// and assign it to themselves at will, so a binding that granted administration
// would make every guild admin a stelfin admin by construction.
func TestAPlatformRoleCannotConferAdmin(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100010")

	if err := s.BindRole(ctx, tn.org.ID, chat.Discord, "999", chat.RoleAdmin); err == nil {
		t.Fatal("a platform role was allowed to confer admin")
	}
}

func TestBalancesAndTreasurySnapshot(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100011")

	tn.deposit(t, s, t.Name(), money.MustParse("250"))

	balances, err := s.Balances(ctx, tn.org.ID, tn.account)
	must(t, err, "balances")
	if len(balances) != 1 || balances[0].Balance != money.MustParse("250") {
		t.Fatalf("balances = %+v", balances)
	}
	if balances[0].Code != "USDC" {
		t.Errorf("code = %q", balances[0].Code)
	}

	// Move it into the treasury so the snapshot has something to report.
	_, err = s.Post(ctx, ledger.PostRequest{
		Org: tn.org.ID, IdempotencyKey: t.Name() + "/to-treasury",
		Kind: ledger.TxSend, OccurredAt: time.Unix(1700000001, 0),
		Postings: []ledger.Posting{
			{Account: tn.account, Asset: tn.usdc, Amount: money.MustParse("-100")},
			{Account: tn.treasury, Asset: tn.usdc, Amount: money.MustParse("100")},
		},
	})
	must(t, err, "fund treasury")

	snapshot, err := s.TreasurySnapshot(ctx, tn.org.ID)
	must(t, err, "treasury snapshot")
	if len(snapshot) != 1 || snapshot[0].Balance != money.MustParse("100") {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot[0].Name != "ops" {
		t.Errorf("treasury name = %q", snapshot[0].Name)
	}
}

func TestHistoryPagesNewestFirst(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100012")

	const posts = 7
	for i := 0; i < posts; i++ {
		_, err := s.Post(ctx, ledger.PostRequest{
			Org: tn.org.ID, IdempotencyKey: fmt.Sprintf("%s/%d", t.Name(), i),
			Kind: ledger.TxDeposit, OccurredAt: time.Unix(1700000000+int64(i), 0),
			Postings: []ledger.Posting{
				{Account: tn.account, Asset: tn.usdc, Amount: money.MustParse("1")},
				{Account: tn.external, Asset: tn.usdc, Amount: money.MustParse("-1")},
			},
		})
		must(t, err, "post")
	}

	// One account's view: one row per post, newest first.
	page, cursor, err := s.History(ctx, tn.org.ID, HistoryFilter{Account: tn.account, Limit: 3})
	must(t, err, "history")
	if len(page) != 3 {
		t.Fatalf("page has %d rows, want 3", len(page))
	}
	if cursor.Zero() {
		t.Fatal("no cursor returned; there are more rows")
	}
	if !page[0].OccurredAt.After(page[2].OccurredAt) {
		t.Errorf("page is not newest-first: %v then %v", page[0].OccurredAt, page[2].OccurredAt)
	}

	seen := map[ledger.TxID]bool{}
	for _, r := range page {
		seen[r.TxID] = true
	}
	next, cursor2, err := s.History(ctx, tn.org.ID, HistoryFilter{
		Account: tn.account, Limit: 3, After: cursor,
	})
	must(t, err, "history page 2")
	for _, r := range next {
		if seen[r.TxID] {
			t.Errorf("transaction %d appeared on both pages", r.TxID)
		}
	}
	if len(next) != 3 || cursor2.Zero() {
		t.Errorf("page 2 has %d rows, cursor zero = %v", len(next), cursor2.Zero())
	}

	// The last page returns no cursor, so a caller knows to stop.
	last, cursor3, err := s.History(ctx, tn.org.ID, HistoryFilter{
		Account: tn.account, Limit: 3, After: cursor2,
	})
	must(t, err, "history page 3")
	if len(last) != 1 || !cursor3.Zero() {
		t.Errorf("last page has %d rows, cursor zero = %v, want 1 and true", len(last), cursor3.Zero())
	}
}

func TestHistoryFilters(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "-100013")

	tn.deposit(t, s, t.Name()+"/a", money.MustParse("5"))
	_, err := s.Post(ctx, ledger.PostRequest{
		Org: tn.org.ID, IdempotencyKey: t.Name() + "/b",
		Kind: ledger.TxSend, OccurredAt: time.Unix(1700000100, 0),
		Postings: []ledger.Posting{
			{Account: tn.account, Asset: tn.usdc, Amount: money.MustParse("-2")},
			{Account: tn.external, Asset: tn.usdc, Amount: money.MustParse("2")},
		},
	})
	must(t, err, "send")

	sends, _, err := s.History(ctx, tn.org.ID, HistoryFilter{
		Account: tn.account, Kinds: []ledger.TxKind{ledger.TxSend},
	})
	must(t, err, "filtered history")
	if len(sends) != 1 || sends[0].Kind != ledger.TxSend {
		t.Fatalf("kind filter returned %+v", sends)
	}

	since, _, err := s.History(ctx, tn.org.ID, HistoryFilter{
		Account: tn.account, Since: time.Unix(1700000050, 0),
	})
	must(t, err, "since history")
	if len(since) != 1 {
		t.Fatalf("since filter returned %d rows, want 1", len(since))
	}
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
