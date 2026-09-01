package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

// testPGPort is this package's own Postgres port. `go test ./...` runs packages
// in parallel, so every package that needs a database must claim a distinct one.
const testPGPort = 54334

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

// fakeReplier records replies instead of sending them.
type fakeReplier struct {
	mu   sync.Mutex
	sent []chat.Reply
}

func (r *fakeReplier) Send(_ context.Context, _ chat.Conversation, _ chat.Actor, m chat.Reply) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

func (r *fakeReplier) replies() []chat.Reply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chat.Reply(nil), r.sent...)
}

func (r *fakeReplier) only(t *testing.T) chat.Reply {
	t.Helper()
	sent := r.replies()
	if len(sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(sent))
	}
	return sent[0]
}

// fakeSender records the free-text messages routed to the payment path.
type fakeSender struct {
	mu         sync.Mutex
	calls      []api.Scope
	linked     []string
	treasuries []string
	// treasuryErr is what PrepareTreasuryLink returns, so a test can stand in
	// for an account that cannot be proved at all.
	treasuryErr error
	challenges  *identity.Challenges
}

func (f *fakeSender) HandleSend(
	_ context.Context, scope api.Scope, _ chat.Inbound, _ api.Replier, _ api.Linker,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scope)
	return nil
}

// PrepareLink records challenge requests. The real one talks to SEP-10; what
// the router is responsible for is refusing before it gets there.
func (f *fakeSender) PrepareLink(
	_ context.Context, _ api.Scope, _ store.IdentityID, address string,
) (*api.LinkChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked = append(f.linked, address)
	return &api.LinkChallenge{
		Address: address, XDR: "AAAAAgAAAAA=", Hash: "cafe",
		NetworkPassphrase: network.TestNetworkPassphrase,
	}, nil
}

// PrepareTreasuryLink records treasury challenge requests. The weight
// arithmetic is the real one's job; the router's is deciding who may ask.
func (f *fakeSender) PrepareTreasuryLink(
	_ context.Context, _ api.Scope, _ store.IdentityID, address string,
) (*api.LinkChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.treasuryErr != nil {
		return nil, f.treasuryErr
	}
	f.treasuries = append(f.treasuries, address)
	return &api.LinkChallenge{
		Address: address, XDR: "AAAAAgAAAAA=", Hash: "beef",
		NetworkPassphrase: network.TestNetworkPassphrase,
		Purpose:           store.PurposeLinkTreasury,
	}, nil
}

func (f *fakeSender) provedTreasuries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.treasuries...)
}

// Challenges reports whether linking is available at all. A fake with none
// configured is how a deployment without a web-auth key behaves.
func (f *fakeSender) Challenges() *identity.Challenges { return f.challenges }

func (f *fakeSender) linkedAddresses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.linked...)
}

func (f *fakeSender) sent() []api.Scope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Scope(nil), f.calls...)
}

// fakeAdmins answers the platform-authority question.
type fakeAdmins struct {
	admin bool
	err   error
}

func (f fakeAdmins) IsSpaceAdmin(context.Context, chat.Conversation, chat.Actor) (bool, error) {
	return f.admin, f.err
}

// stubLinker mints predictable links.
type stubLinker struct{}

func (stubLinker) IssueConfirmLink(_ api.Scope, hash string, _ time.Time) (string, error) {
	return "https://stelfin.example/confirm#" + hash, nil
}

func (stubLinker) IssueEnrollLink(scope api.Scope, _ time.Time) (string, error) {
	return "https://stelfin.example/enroll#" + scope.OwnerRef, nil
}

func (stubLinker) IssueLinkLink(_ api.Scope, hash string, _ time.Time) (string, error) {
	return "https://stelfin.example/link#" + hash, nil
}

type harness struct {
	svc    *Service
	store  *store.Store
	sender *fakeSender
	out    *fakeReplier
}

func newHarness(t *testing.T, admin bool) *harness {
	t.Helper()
	db := store.New(testPool)
	challenges, err := identity.New(identity.Config{
		Seed:              keypair.MustRandom().Seed(),
		HomeDomain:        "stelfin.test",
		WebAuthDomain:     "stelfin.test",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("challenges: %v", err)
	}
	sender := &fakeSender{challenges: challenges}
	svc, err := New(Config{
		Store: db, Sender: sender, Admins: fakeAdmins{admin: admin}, Network: "testnet",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &harness{svc: svc, store: db, sender: sender, out: &fakeReplier{}}
}

func (h *harness) handle(t *testing.T, m chat.Inbound) error {
	t.Helper()
	return h.svc.Handle(context.Background(), m, h.out, stubLinker{})
}

var msgSeq int

// message builds an inbound message in a group.
func message(space, command, args string) chat.Inbound {
	msgSeq++
	return chat.Inbound{
		DedupeID: fmt.Sprintf("telegram:%d", msgSeq),
		Actor: chat.Actor{
			Channel: chat.Telegram, UserID: "42", Handle: "ada",
		},
		Conversation: chat.Conversation{Channel: chat.Telegram, SpaceID: space},
		Command:      command,
		Args:         args,
		ReceivedAt:   time.Now(),
	}
}

// TestUnregisteredSpaceIsSilent is the tenancy rule at its most consequential.
//
// The bot has been added somewhere nobody ran setup. There is no org to be a
// member of and no books to read, so anything other than setup gets no reply at
// all — not an error, not an explanation. Answering would be talking to a room
// that never asked, in a group that may not have wanted the bot in the first
// place.
func TestUnregisteredSpaceIsSilent(t *testing.T) {
	h := newHarness(t, true)

	for _, m := range []chat.Inbound{
		message("-999001", "", "send 5000 to ada"),
		message("-999001", "whoami", ""),
		message("-999001", "help", ""),
	} {
		if err := h.handle(t, m); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if got := len(h.out.replies()); got != 0 {
		t.Fatalf("replied %d times in a space with no org", got)
	}
	if got := len(h.sender.sent()); got != 0 {
		t.Fatalf("routed %d messages to the payment path in a space with no org", got)
	}
}

// TestUnregisteredSpaceCostsNothing: with no org, nothing is claimed either. A
// message that was ignored must not consume its delivery id, or setup could
// never run after a stray message from the same delivery.
func TestUnregisteredSpaceCostsNothing(t *testing.T) {
	h := newHarness(t, true)

	m := message("-999002", "whoami", "")
	if err := h.handle(t, m); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var claimed int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_messages WHERE id = $1`, m.DedupeID).Scan(&claimed); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claimed != 0 {
		t.Error("an ignored message consumed its delivery id")
	}
}

func TestSetupCreatesAWorkspace(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if err := h.handle(t, message("-999003", commandSetup, "Acme DAO")); err != nil {
		t.Fatalf("setup: %v", err)
	}

	body := h.out.only(t).Text
	if !strings.Contains(body, "Acme DAO") {
		t.Errorf("reply does not name the workspace:\n%s", body)
	}

	org, ok, err := h.store.OrgForSpace(ctx, chat.Telegram, "-999003")
	if err != nil || !ok {
		t.Fatalf("org for space: %v (found %v)", err, ok)
	}
	if org.DisplayName != "Acme DAO" || org.Network != "testnet" {
		t.Fatalf("org = %+v", org)
	}

	// Whoever set it up is its first admin: somebody has to be, and the
	// alternative is a workspace nobody can administer.
	member, ok, err := h.store.MemberByIdentity(ctx, org.ID, chat.Telegram, "42")
	if err != nil || !ok {
		t.Fatalf("member: %v (found %v)", err, ok)
	}
	role, err := h.store.EffectiveRole(ctx, org.ID, member.ID, chat.Telegram, nil)
	if err != nil {
		t.Fatalf("effective role: %v", err)
	}
	if role != chat.RoleAdmin {
		t.Errorf("the person who ran setup holds %s, want admin", role)
	}
}

// TestSetupNeedsPlatformAuthority: setup runs before any org role can exist, so
// its gate is authority over the space itself.
func TestSetupNeedsPlatformAuthority(t *testing.T) {
	h := newHarness(t, false)

	if err := h.handle(t, message("-999004", commandSetup, "Not Mine")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "administrator") {
		t.Errorf("reply does not explain the refusal:\n%s", body)
	}

	if _, ok, err := h.store.OrgForSpace(context.Background(), chat.Telegram, "-999004"); err != nil || ok {
		t.Fatal("a workspace was created by someone with no authority over the space")
	}
}

// TestSetupRefusesWhenAuthorityCannotBeEstablished: not knowing must fail
// closed. Refusing a command is recoverable; creating a workspace for the wrong
// person is not.
func TestSetupRefusesWhenAuthorityCannotBeEstablished(t *testing.T) {
	db := store.New(testPool)
	svc, err := New(Config{
		Store: db, Sender: &fakeSender{},
		Admins:  fakeAdmins{admin: true, err: errors.New("platform unreachable")},
		Network: "testnet",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	out := &fakeReplier{}

	if err := svc.Handle(context.Background(),
		message("-999005", commandSetup, "Acme"), out, stubLinker{}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if _, ok, err := db.OrgForSpace(context.Background(), chat.Telegram, "-999005"); err != nil || ok {
		t.Fatal("a workspace was created while authority was unknown")
	}
}

func TestSetupTwiceIsANoOp(t *testing.T) {
	h := newHarness(t, true)

	if err := h.handle(t, message("-999006", commandSetup, "First")); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if err := h.handle(t, message("-999006", commandSetup, "Second")); err != nil {
		t.Fatalf("second setup: %v", err)
	}

	replies := h.out.replies()
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}
	if !strings.Contains(replies[1].Text, "already set up") {
		t.Errorf("second setup was not reported as a no-op:\n%s", replies[1].Text)
	}

	org, _, err := h.store.OrgForSpace(context.Background(), chat.Telegram, "-999006")
	if err != nil {
		t.Fatalf("org for space: %v", err)
	}
	if org.DisplayName != "First" {
		t.Errorf("the workspace was renamed to %q by a second setup", org.DisplayName)
	}
}

func TestSetupRefusesInADirectMessage(t *testing.T) {
	h := newHarness(t, true)

	m := message("999007", commandSetup, "Acme")
	m.Conversation.IsDM = true
	if err := h.handle(t, m); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "direct message") {
		t.Errorf("reply does not explain:\n%s", body)
	}
}

func TestSetupNeedsAName(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999008", commandSetup, "   ")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "call this workspace") {
		t.Errorf("reply does not ask for a name:\n%s", body)
	}
}

// TestClaimedExactlyOnce: platforms retry, and one instruction must not become
// two of anything.
func TestClaimedExactlyOnce(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999009", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h.out = &fakeReplier{}

	m := message("-999009", "whoami", "")
	for i := 0; i < 3; i++ {
		if err := h.handle(t, m); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if got := len(h.out.replies()); got != 1 {
		t.Fatalf("replied %d times to 3 deliveries of one message", got)
	}
}

// TestConcurrentDeliveriesActOnce: two retries arriving together must not both
// win the claim.
func TestConcurrentDeliveriesActOnce(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999010", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h.out = &fakeReplier{}

	m := message("-999010", "whoami", "")
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.svc.Handle(context.Background(), m, h.out, stubLinker{}); err != nil {
				t.Errorf("handle: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(h.out.replies()); got != 1 {
		t.Errorf("replied %d times to %d concurrent deliveries, want 1", got, workers)
	}
}

// TestDedupeIsChannelScoped: the same numeric id on two platforms is two
// different messages from two different people.
func TestDedupeIsChannelScoped(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999011", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h.out = &fakeReplier{}

	// Ids chosen not to collide with the generated ones: the point is that the
	// same number on two platforms is two messages, not that any number is free.
	base := message("-999011", "whoami", "")
	for _, id := range []string{"telegram:same-number-7", "discord:same-number-7"} {
		m := base
		m.DedupeID = id
		if err := h.handle(t, m); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if got := len(h.out.replies()); got != 2 {
		t.Errorf("replied %d times to two messages from different platforms, want 2", got)
	}
}

func TestMessageWithNoDedupeIDIsRefused(t *testing.T) {
	h := newHarness(t, true)
	m := message("-999012", "whoami", "")
	m.DedupeID = ""

	if err := h.handle(t, m); err == nil {
		t.Fatal("a message with no delivery id was accepted")
	}
	if got := len(h.out.replies()); got != 0 {
		t.Errorf("replied %d times to a message that should not have been processed", got)
	}
}

// TestFreeTextGoesToThePaymentPath, scoped to the org the space belongs to.
func TestFreeTextGoesToThePaymentPath(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999013", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := h.handle(t, message("-999013", "", "send 5000 to ada")); err != nil {
		t.Fatalf("free text: %v", err)
	}

	sent := h.sender.sent()
	if len(sent) != 1 {
		t.Fatalf("routed %d messages to the payment path, want 1", len(sent))
	}
	org, _, err := h.store.OrgForSpace(context.Background(), chat.Telegram, "-999013")
	if err != nil {
		t.Fatalf("org for space: %v", err)
	}
	if sent[0].Org != org.ID || sent[0].OwnerRef != "telegram:42" {
		t.Errorf("scope = %+v, want the space's org and the actor's ref", sent[0])
	}
}

func TestUnknownCommandIsExplained(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999014", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-999014", "launch", "")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "/help") {
		t.Errorf("reply does not point anywhere useful:\n%s", body)
	}
	if got := len(h.sender.sent()); got != 0 {
		t.Error("an unknown command fell through to the payment path")
	}
}

// TestEveryReplyIsEphemeral is the group-chat rule. Replies carry authority
// links or quote someone's own instruction back at them; neither belongs in a
// room several hundred people can read.
func TestEveryReplyIsEphemeral(t *testing.T) {
	h := newHarness(t, true)

	for _, m := range []chat.Inbound{
		message("-999015", commandSetup, "Acme"),
		message("-999015", "whoami", ""),
		message("-999015", "help", ""),
		message("-999015", "nonsense", ""),
	} {
		if err := h.handle(t, m); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	replies := h.out.replies()
	if len(replies) == 0 {
		t.Fatal("no replies to check")
	}
	for i, r := range replies {
		if !r.Ephemeral {
			t.Errorf("reply %d was not ephemeral:\n%s", i, r.Text)
		}
	}
}

// TestSuspendedWorkspaceMovesNoMoney: a suspended tenant keeps its history
// readable and its payment path closed.
func TestSuspendedWorkspaceMovesNoMoney(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	if err := h.handle(t, message("-999016", commandSetup, "Acme")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	org, _, err := h.store.OrgForSpace(ctx, chat.Telegram, "-999016")
	if err != nil {
		t.Fatalf("org for space: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE orgs SET status = 'suspended' WHERE id = $1`, int64(org.ID)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-999016", "", "send 5000 to ada")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := len(h.sender.sent()); got != 0 {
		t.Fatal("a suspended workspace reached the payment path")
	}
	if body := h.out.only(t).Text; !strings.Contains(body, "suspended") {
		t.Errorf("reply does not say why:\n%s", body)
	}
}

func TestWhoamiReportsWhereYouStand(t *testing.T) {
	h := newHarness(t, true)
	if err := h.handle(t, message("-999017", commandSetup, "Acme DAO")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h.out = &fakeReplier{}

	if err := h.handle(t, message("-999017", "whoami", "")); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	body := h.out.only(t).Text
	for _, want := range []string{"Acme DAO", "telegram:42", "admin", "not linked"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}
	// The distinction the whole model rests on has to be said, not implied.
	if !strings.Contains(body, "on chain") {
		t.Errorf("reply does not distinguish a role from a signature:\n%s", body)
	}
}

func TestSlugify(t *testing.T) {
	for name, want := range map[string]string{
		"Acme DAO":          "acme-dao-999",
		"  Spaces   Here  ": "spaces-here-999",
		// Anything outside [a-z0-9] is a separator, including letters this
		// function cannot render. A name reducing to nothing still has to
		// produce a usable slug, and the space id makes it unique.
		"Ünïcödé": "n-c-d-999",
		"":        "dao-999",
		"!!!":     "dao-999",
		"A very long name that runs well past the limit": "a-very-long-name-that-ru-999",
	} {
		if got := slugify(name, "-999"); got != want {
			t.Errorf("slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestSlugsAreUniquePerSpace: two DAOs both called "Treasury" are ordinary, and
// the slug is a unique key.
func TestSlugsAreUniquePerSpace(t *testing.T) {
	a := slugify("Treasury", "-1001")
	b := slugify("Treasury", "-1002")
	if a == b {
		t.Fatalf("two spaces produced the same slug: %q", a)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	db := store.New(testPool)
	full := Config{Store: db, Sender: &fakeSender{}, Admins: fakeAdmins{}, Network: "testnet"}

	for name, mutate := range map[string]func(*Config){
		"no store":    func(c *Config) { c.Store = nil },
		"no sender":   func(c *Config) { c.Sender = nil },
		"no admins":   func(c *Config) { c.Admins = nil },
		"bad network": func(c *Config) { c.Network = "futurenet" },
	} {
		cfg := full
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
