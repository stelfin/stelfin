package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/egress"
	"github.com/stelfin/stelfin/connector/egress/egresstest"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/mcp"
)

// connectorFixture is a workspace with a 2-of-2 treasury and a deployment
// configured for connectors, reaching one local TLS server through the same
// egress client a real endpoint would be reached through.
type connectorFixture struct {
	*proposalFixture
	server *httptest.Server
	// hits counts requests that reached the connector's server.
	hits atomic.Int32
}

func newConnectorFixture(t *testing.T, name string, handler http.Handler) *connectorFixture {
	t.Helper()
	f := &connectorFixture{proposalFixture: newProposalFixture(t, name)}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)

	sealer, err := connector.NewSealer([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	cfg := &ConnectorConfig{Sealer: sealer, Egress: egresstest.Client(t, f.server, egress.Options{})}
	guard, err := connector.NewGuard(grants{store: f.svc.store}, nil)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	f.svc.connectors, f.svc.guard = cfg, guard
	return f
}

// payrollRows serves a small payroll as a webhook would, and refuses anyone
// who does not present the token.
func payrollRows(token string, rows *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, *rows)
	})
}

// grant walks a connector's grant to completion: asks, signs with both of the
// treasury's signers, and submits.
func (f *connectorFixture) grant(t *testing.T, name string, perHour int) *GrantResult {
	t.Helper()
	ctx := context.Background()
	request, err := f.svc.PrepareGrant(ctx, f.scope, f.identity, f.member, name, f.treasuryRow.ID, perHour)
	if err != nil {
		t.Fatalf("PrepareGrant: %v", err)
	}
	done, err := f.svc.SubmitGrant(ctx, f.scope, request.Challenge.Hash,
		signChallengeWith(t, request.Challenge.XDR, f.treasury, f.co))
	if err != nil {
		t.Fatalf("SubmitGrant: %v", err)
	}
	return done
}

// TestAConnectorIsUsedOnlyOnTheSignersSayso is the property the whole layer
// exists for, from one end to the other and with real connector code: nothing
// an admin can do alone makes stelfin call a connector. It takes the treasury's
// signatures, at the weight that spends, over a statement of what is granted.
func TestAConnectorIsUsedOnlyOnTheSignersSayso(t *testing.T) {
	ada, bo := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	rows := fmt.Sprintf(`[["address","amount","memo"],[%q,"100","march"],[%q,"50.5",""]]`, ada, bo)
	f := newConnectorFixture(t, "connector grant", payrollRows("tok-123", &rows))
	ctx := context.Background()
	secretURL := f.server.URL + "/hooks/s3cret-path"

	row, err := f.svc.RegisterConnector(ctx, f.scope, f.member, "payroll", store.ConnectorWebhook)
	if err != nil {
		t.Fatalf("RegisterConnector: %v", err)
	}
	if row.Ready {
		t.Fatal("a webhook with no address is ready")
	}
	if _, err := f.svc.PrepareGrant(ctx, f.scope, f.identity, f.member, "payroll", f.treasuryRow.ID, 0); !errors.Is(err, ErrConnectorNotReady) {
		t.Fatalf("a connector with no address was put to the signers: %v", err)
	}

	// Set-up tries it. A wrong token is found here, by the person typing it.
	if _, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, secretURL, "wrong"); !errors.Is(err, ErrConnectorFailed) {
		t.Fatalf("set-up with a wrong token: %v", err)
	}
	setup, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, secretURL, "tok-123")
	if err != nil {
		t.Fatalf("SetUpConnector: %v", err)
	}
	if setup.Rows != 3 {
		t.Errorf("set-up saw %d rows, want 3", setup.Rows)
	}

	// Neither the path nor the token is anywhere a reader of the database, or
	// of a statement, could find it.
	var stored string
	if err := testPool.QueryRow(ctx,
		`SELECT sealed_endpoint || '|' || sealed_credential || '|' || endpoint_host FROM connectors WHERE id = $1`,
		int64(row.ID)).Scan(&stored); err != nil {
		t.Fatalf("read connector row: %v", err)
	}
	for _, secret := range []string{"s3cret-path", "tok-123"} {
		if strings.Contains(stored, secret) {
			t.Fatalf("the connector row holds %q in the clear: %s", secret, stored)
		}
	}

	// Set up, and still not usable.
	before := f.hits.Load()
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "payroll", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("read before any grant: %v", err)
	}
	if f.hits.Load() != before {
		t.Fatal("an ungranted connector was called")
	}

	request, err := f.svc.PrepareGrant(ctx, f.scope, f.identity, f.member, "payroll", f.treasuryRow.ID, 30)
	if err != nil {
		t.Fatalf("PrepareGrant: %v", err)
	}
	for _, want := range []string{"connector\tpayroll", "kind\twebhook", "may\tread.rows", "rate\t30 calls an hour", f.treasury.Address()} {
		if !strings.Contains(request.Statement, want) {
			t.Errorf("the statement does not say %q:\n%s", want, request.Statement)
		}
	}
	for _, secret := range []string{"s3cret-path", "tok-123"} {
		if strings.Contains(request.Statement, secret) {
			t.Fatalf("the statement shown to every signer holds %q", secret)
		}
	}

	// Asking is not granting.
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "payroll", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("read after a request and before any signature: %v", err)
	}
	// One signature of the two a payment needs is not the treasury.
	one := signChallengeWith(t, request.Challenge.XDR, f.treasury)
	if _, err := f.svc.SubmitGrant(ctx, f.scope, request.Challenge.Hash, one); err == nil {
		t.Fatal("one signature of two put a grant in force")
	}
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "payroll", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("read after one signature of two: %v", err)
	}

	both := signChallengeWith(t, request.Challenge.XDR, f.treasury, f.co)
	done, err := f.svc.SubmitGrant(ctx, f.scope, request.Challenge.Hash, both)
	if err != nil {
		t.Fatalf("SubmitGrant with both signatures: %v", err)
	}
	if len(done.Grant.SignedBy) != 2 || done.Grant.PerHour != 30 {
		t.Fatalf("grant = %+v", done.Grant)
	}
	// A signed challenge is spent once.
	if _, err := f.svc.SubmitGrant(ctx, f.scope, request.Challenge.Hash, both); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("the same signed challenge was accepted twice: %v", err)
	}

	// The evidence is kept, and stands on its own: the stored envelope still
	// verifies against the signers and the stored statement's digest.
	kept, err := f.svc.store.GrantInForce(ctx, f.scope.Org, "payroll")
	if err != nil {
		t.Fatalf("GrantInForce: %v", err)
	}
	if digestOf(kept.Statement) != kept.StatementDigest {
		t.Fatal("the stored statement does not hash to the stored digest")
	}
	if _, err := f.svc.challenges.VerifyStatement(kept.SignedXDR, 2,
		txnbuild.SignerSummary{f.treasury.Address(): 1, f.co.Address(): 1}, kept.StatementDigest); err != nil {
		t.Fatalf("the stored evidence does not verify: %v", err)
	}

	seen, _, err := f.svc.ReadConnector(ctx, f.scope, "payroll", "")
	if err != nil {
		t.Fatalf("read once granted: %v", err)
	}
	if len(seen.Rows) != 3 {
		t.Fatalf("read %d rows, want 3", len(seen.Rows))
	}

	// Another workspace has no connector of that name, and no way to this one.
	elsewhere := Scope{Org: orgFor(t, "/elsewhere").ID, OwnerRef: f.scope.OwnerRef}
	if _, _, err := f.svc.ReadConnector(ctx, elsewhere, "payroll", ""); !errors.Is(err, store.ErrNoConnector) {
		t.Fatalf("another workspace reached this connector: %v", err)
	}

	// Setting it up again changes what the signers agreed to, so their grant
	// does not carry over to whatever it now is.
	if _, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, secretURL, "tok-123"); err != nil {
		t.Fatalf("set up again: %v", err)
	}
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "payroll", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("a grant survived the connector being set up again: %v", err)
	}
}

// TestAGrantCannotBeMadeFromSomethingElseThatWasSigned: the treasury's signers
// sign several kinds of challenge. Each proves one thing, and none of them may
// be presented as a grant — nor a grant's signatures moved to a different one.
func TestAGrantCannotBeMadeFromSomethingElseThatWasSigned(t *testing.T) {
	rows := `[["a","1"]]`
	f := newConnectorFixture(t, "connector replay", payrollRows("", &rows))
	ctx := context.Background()
	row, err := f.svc.RegisterConnector(ctx, f.scope, f.member, "feed", store.ConnectorWebhook)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, f.server.URL+"/", ""); err != nil {
		t.Fatalf("set up: %v", err)
	}

	// A proof that the group controls the treasury, signed at full weight.
	link, err := f.svc.PrepareTreasuryLink(ctx, f.scope, f.identity, f.treasury.Address())
	if err != nil {
		t.Fatalf("PrepareTreasuryLink: %v", err)
	}
	proof := signChallengeWith(t, link.XDR, f.treasury, f.co)

	first, err := f.svc.PrepareGrant(ctx, f.scope, f.identity, f.member, "feed", f.treasuryRow.ID, 10)
	if err != nil {
		t.Fatalf("PrepareGrant: %v", err)
	}
	// The proof of control, presented as the grant.
	if _, err := f.svc.SubmitGrant(ctx, f.scope, first.Challenge.Hash, proof); err == nil {
		t.Fatal("a proof of control was accepted as a grant")
	}
	// And the grant challenge, presented as a treasury link.
	firstSigned := signChallengeWith(t, first.Challenge.XDR, f.treasury, f.co)
	if _, err := f.svc.SubmitTreasuryLink(ctx, f.scope, first.Challenge.Hash, firstSigned, ""); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("a grant challenge was accepted as a treasury link: %v", err)
	}

	// The signers agreed to ten calls an hour. Asking again for a thousand
	// replaces the request; their signatures stay with the one they signed.
	second, err := f.svc.PrepareGrant(ctx, f.scope, f.identity, f.member, "feed", f.treasuryRow.ID, 1000)
	if err != nil {
		t.Fatalf("PrepareGrant again: %v", err)
	}
	if _, err := f.svc.SubmitGrant(ctx, f.scope, first.Challenge.Hash, firstSigned); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("signatures over a superseded request were accepted: %v", err)
	}
	if _, err := f.svc.SubmitGrant(ctx, f.scope, second.Challenge.Hash, firstSigned); err == nil {
		t.Fatal("signatures over ten calls an hour granted a thousand")
	}
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "feed", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("something was granted along the way: %v", err)
	}

	// Revoking takes nobody's signature, and takes effect at once.
	f.grant(t, "feed", 10)
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "feed", ""); err != nil {
		t.Fatalf("read once granted: %v", err)
	}
	if err := f.svc.store.RevokeGrant(ctx, f.scope.Org, "feed", f.member); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "feed", ""); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("read after revocation: %v", err)
	}
}

// TestASheetBecomesAPayrollBySteps: read, draft, review, propose — and the
// proposal is what was drafted, even if the source has changed since.
func TestASheetBecomesAPayrollBySteps(t *testing.T) {
	ada, bo := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	rows := fmt.Sprintf(`[["address","amount","memo"],[%q,"100","march"],[%q,"50.5",""],["",""]]`, ada, bo)
	f := newConnectorFixture(t, "connector payroll", payrollRows("", &rows))
	ctx := context.Background()
	row, err := f.svc.RegisterConnector(ctx, f.scope, f.member, "payroll", store.ConnectorWebhook)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, f.server.URL+"/", ""); err != nil {
		t.Fatalf("set up: %v", err)
	}
	f.grant(t, "payroll", 0)

	draft, err := f.svc.DraftPayroll(ctx, f.scope, f.member, "payroll", "", "March salaries")
	if err != nil {
		t.Fatalf("DraftPayroll: %v", err)
	}
	if len(draft.Rows) != 2 || draft.Total != money.MustParse("150.5") {
		t.Fatalf("draft = %d rows totalling %s, want 2 totalling 150.5", len(draft.Rows), draft.Total)
	}
	// Drafting proposed nothing.
	if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
		t.Fatal("drafting opened a proposal")
	}

	// The sheet changes after the draft was reviewed: a third person, a
	// thousand times the money.
	eve := keypair.MustRandom().Address()
	rows = fmt.Sprintf(`[["address","amount"],[%q,"100000"],[%q,"50.5"],[%q,"999999"]]`, ada, bo, eve)

	view, _, err := f.svc.ProposeDraft(ctx, f.scope, draft.ID, f.treasuryRow.ID, f.member)
	if err != nil {
		t.Fatalf("ProposeDraft: %v", err)
	}
	canonical := view.Description.Canonical()
	if strings.Contains(canonical, eve) || strings.Contains(canonical, "100000") {
		t.Fatalf("the proposal was read from the sheet again, not from the draft:\n%s", canonical)
	}
	for _, want := range []string{ada, bo, "100.0000000", "50.5000000"} {
		if !strings.Contains(canonical, want) {
			t.Errorf("the proposal does not carry %q:\n%s", want, canonical)
		}
	}
	if _, _, err := f.svc.ProposeDraft(ctx, f.scope, draft.ID, f.treasuryRow.ID, f.member); !errors.Is(err, store.ErrNoDraft) {
		t.Fatalf("a draft was proposed twice: %v", err)
	}

	// A sheet with one row that cannot be read drafts nothing at all.
	rows = fmt.Sprintf(`[["address","amount"],[%q,"100"],["not an address","5"]]`, ada)
	_, err = f.svc.DraftPayroll(ctx, f.scope, f.member, "payroll", "", "")
	var unreadable *BatchError
	if !errors.As(err, &unreadable) || len(unreadable.Rows) != 1 || unreadable.Rows[0].Line != 3 {
		t.Fatalf("error = %v, want row 3 named", err)
	}
	// And a memo that would be cut is refused rather than cut.
	rows = fmt.Sprintf(`[["address","amount"],[%q,"100"]]`, ada)
	if _, err := f.svc.DraftPayroll(ctx, f.scope, f.member, "payroll", "", strings.Repeat("m", 29)); !errors.Is(err, ErrMemoTooLong) {
		t.Fatalf("a 29-byte memo: %v", err)
	}
}

// TestStelfinsMCPClientReadsStelfinsMCPServer points one half of this codebase
// at the other: the client a workspace would use to reach somebody's MCP
// server, against the server stelfin itself exposes to agents. Two
// implementations of one protocol, written separately, agreeing.
func TestStelfinsMCPClientReadsStelfinsMCPServer(t *testing.T) {
	db := store.New(testPool)
	agents, err := mcp.NewServer(db, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("mcp server: %v", err)
	}
	f := newConnectorFixture(t, "connector mcp", agents.Handler())
	ctx := context.Background()

	// The token an agent would be given for this same workspace.
	token, _, err := db.IssueMCPToken(ctx, f.scope.Org, "loopback", store.TierRead, f.identity, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	row, err := f.svc.RegisterConnector(ctx, f.scope, f.member, "books", store.ConnectorMCP)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	setup, err := f.svc.SetUpConnector(ctx, f.scope, row.ID, f.server.URL+"/mcp", token)
	if err != nil {
		t.Fatalf("SetUpConnector: %v", err)
	}
	if len(setup.Tools) == 0 || !contains(setup.Tools, "stelfin_treasuries") {
		t.Fatalf("discovered %v", setup.Tools)
	}

	done := f.grant(t, "books", 0)
	// The grant names each tool, so the signers saw what could be called.
	for _, tool := range setup.Tools {
		if !strings.Contains(done.Grant.Statement, "may\ttool."+tool) {
			t.Errorf("the statement does not name %s:\n%s", tool, done.Grant.Statement)
		}
	}

	seen, _, err := f.svc.ReadConnector(ctx, f.scope, "books", "stelfin_treasuries")
	if err != nil {
		t.Fatalf("ReadConnector: %v", err)
	}
	var text strings.Builder
	for _, r := range seen.Rows {
		for _, c := range r {
			text.WriteString(c.Unwrap())
		}
	}
	if !strings.Contains(text.String(), f.treasury.Address()) {
		t.Fatalf("the tool's answer does not name the treasury:\n%s", text.String())
	}

	// A tool that is not in the pinned surface is not asked for.
	before := f.hits.Load()
	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "books", "transfer_everything"); err == nil {
		t.Fatal("a tool outside the grant was called")
	}
	if f.hits.Load() != before {
		t.Fatal("the server was contacted about a tool that was never granted")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func cellsOf(texts ...string) []connector.Untrusted[string] {
	out := make([]connector.Untrusted[string], len(texts))
	for i, s := range texts {
		out[i] = connector.Wrap(s)
	}
	return out
}

// A table is read as a payroll by its headings where it has them and by
// position where it does not — and a table with headings this does not know is
// refused, so that a sheet of employee ids and start dates is never paid.
func TestRowsAreReadAsAPayrollOnlyWhenTheyAreOne(t *testing.T) {
	type cell = []connector.Untrusted[string]

	positional, err := draftFromRows(connector.Observation{Rows: []cell{
		cellsOf("GABC", "100", "march"), cellsOf("", ""), cellsOf("GDEF", "5"),
	}})
	if err != nil {
		t.Fatalf("positional: %v", err)
	}
	if len(positional.Rows) != 2 || positional.Rows[1].Line != 3 ||
		positional.Rows[0].DestinationText.Unwrap() != "GABC" || positional.Rows[0].MemoText.Unwrap() != "march" {
		t.Fatalf("positional = %+v", positional.Rows)
	}

	// Headings in any order, as the first row or as Columns, with extra columns.
	for name, seen := range map[string]connector.Observation{
		"first row": {Rows: []cell{cellsOf("Name", "Amount", "Wallet"), cellsOf("Ada", "100", "GABC")}},
		"columns":   {Columns: cellsOf("name", "AMOUNT", "address"), Rows: []cell{cellsOf("Ada", "100", "GABC")}},
	} {
		d, err := draftFromRows(seen)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(d.Rows) != 1 || d.Rows[0].DestinationText.Unwrap() != "GABC" || d.Rows[0].AmountText.Unwrap() != "100" {
			t.Errorf("%s = %+v", name, d.Rows)
		}
	}

	if _, err := draftFromRows(connector.Observation{
		Columns: cellsOf("employee id", "start date"), Rows: []cell{cellsOf("17", "2026-01-01")},
	}); !errors.Is(err, ErrNotTabular) {
		t.Errorf("unknown headings: %v", err)
	}
	if _, err := draftFromRows(connector.Observation{Rows: []cell{cellsOf("", "")}}); !errors.Is(err, ErrBatchEmpty) {
		t.Errorf("no rows: %v", err)
	}
}

// The statement is the thing signed. It must not be possible to add a line to
// it through a name somebody chose.
func TestAStatementCannotBeExtendedThroughAName(t *testing.T) {
	statement := grantStatement(
		store.Org{ID: 7, DisplayName: "Acme\nmay\tsign.everything", Network: "testnet"},
		store.Treasury{Address: "GTREASURY"},
		store.Connector{Name: "payroll", Kind: store.ConnectorSheets},
		connector.Descriptor{ID: "payroll", Kind: connector.KindReader, Capabilities: []string{"read.range"}},
		60)
	may := 0
	for _, line := range strings.Split(statement, "\n") {
		if strings.HasPrefix(line, "may\t") {
			may++
		}
	}
	if may != 1 {
		t.Fatalf("a workspace name added a capability line:\n%s", statement)
	}
}

// fixedToken stands in for the service-account exchange.
type fixedToken struct{}

func (fixedToken) Token(context.Context, int64) (string, error) { return "ya29.test", nil }

// toServer sends every request to a test server, keeping the path. The Sheets
// endpoint is a constant, so this is how a test stands in for Google.
type toServer struct{ host string }

func (rt toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = "http", rt.host
	return http.DefaultTransport.RoundTrip(clone)
}

// TestTwoSpreadsheetConnectorsDoNotShareAGrant: every Sheets reader describes
// itself as "sheets", and a grant is recorded against the name a workspace
// gave. If the guard looked a grant up under the kind, granting one
// spreadsheet connector would grant them all.
func TestTwoSpreadsheetConnectorsDoNotShareAGrant(t *testing.T) {
	ada := keypair.MustRandom().Address()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// A number as Sheets sends one: unquoted. It must reach the batch with
		// its digits intact and never through a float.
		fmt.Fprintf(w, `{"range":"A1:B2","values":[["address","amount"],[%q, 1000.5000001]]}`, ada)
	}))
	t.Cleanup(google.Close)

	f := newConnectorFixture(t, "connector sheets", http.NotFoundHandler())
	f.svc.connectors.Sheets = fixedToken{}
	f.svc.connectors.SheetsHTTP = &http.Client{Transport: toServer{host: strings.TrimPrefix(google.URL, "http://")}}
	ctx := context.Background()

	for _, name := range []string{"payroll", "grants"} {
		row, err := f.svc.RegisterConnector(ctx, f.scope, f.member, name, store.ConnectorSheets)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if !row.Ready {
			t.Fatalf("a spreadsheet connector needs set-up")
		}
	}
	done := f.grant(t, "payroll", 0)
	if !strings.Contains(done.Grant.Statement, "reaches\tGoogle Sheets") {
		t.Errorf("statement:\n%s", done.Grant.Statement)
	}

	if _, _, err := f.svc.ReadConnector(ctx, f.scope, "grants", "SHEET!A1:B2"); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("an ungranted spreadsheet connector was read on another's grant: %v", err)
	}

	draft, err := f.svc.DraftPayroll(ctx, f.scope, f.member, "payroll", "SHEET!A1:B2", "")
	if err != nil {
		t.Fatalf("DraftPayroll from a sheet: %v", err)
	}
	if draft.Total != money.MustParse("1000.5000001") {
		t.Fatalf("total = %s, want exactly 1000.5000001", draft.Total)
	}
}

// TestAnAgentDraftsAndCannotDoMore: a propose-tier token writes a draft, marked
// as the agent's, through the same reading as a member's sheet — and no draft,
// proposal or payment comes of a read-tier token, of amounts sent as numbers,
// or of a saved name in place of an address.
func TestAnAgentDraftsAndCannotDoMore(t *testing.T) {
	f := newProposalFixture(t, "agent drafts")
	ctx := context.Background()
	db := store.New(testPool)
	agents, err := mcp.NewServer(db, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("mcp server: %v", err)
	}
	agents = agents.WithDrafter(f.svc)
	srv := httptest.NewServer(agents.Handler())
	t.Cleanup(srv.Close)

	proposeToken, _, err := db.IssueMCPToken(ctx, f.scope.Org, "payroll-bot", store.TierPropose, f.identity, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	readToken, _, err := db.IssueMCPToken(ctx, f.scope.Org, "reporting", store.TierRead, f.identity, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	callTool := func(token, arguments string) string {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stelfin_draft_payments","arguments":` + arguments + `}}`
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}
	ada, bo := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	good := fmt.Sprintf(`{"payments":[{"to":%q,"amount":"1250.5000001"},{"to":%q,"amount":"10"}],"memo":"Q3 grants"}`, ada, bo)

	// A read token does not reach the tool at all.
	if answer := callTool(readToken, good); !strings.Contains(answer, "no tool") {
		t.Fatalf("a read token reached the drafting tool: %s", answer)
	}

	for name, arguments := range map[string]string{
		"an amount as a number": fmt.Sprintf(`{"payments":[{"to":%q,"amount":1250.5000001}]}`, ada),
		"a saved name":          `{"payments":[{"to":"mum","amount":"5"}]}`,
		"an unreadable amount":  fmt.Sprintf(`{"payments":[{"to":%q,"amount":"lots"}]}`, ada),
		"a long memo":           fmt.Sprintf(`{"payments":[{"to":%q,"amount":"5"}],"memo":%q}`, ada, strings.Repeat("m", 29)),
	} {
		if answer := callTool(proposeToken, arguments); !strings.Contains(answer, `"isError":true`) {
			t.Errorf("%s was drafted: %s", name, answer)
		}
	}
	if drafts, _ := db.Drafts(ctx, f.scope.Org, 10); len(drafts) != 0 {
		t.Fatalf("%d drafts were written from refused calls", len(drafts))
	}

	answer := callTool(proposeToken, good)
	if !strings.Contains(answer, `\"rows\": 2`) || !strings.Contains(answer, "1260.5000001") {
		t.Fatalf("draft answer: %s", answer)
	}
	drafts, err := db.Drafts(ctx, f.scope.Org, 10)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("drafts: %v (%d)", err, len(drafts))
	}
	d := drafts[0]
	if d.AgentLabel != "payroll-bot" || d.CreatedBy != 0 || d.Total != money.MustParse("1260.5000001") {
		t.Fatalf("draft = %+v", d)
	}
	// Writing a draft opened nothing.
	if _, open, _ := f.store.OpenProposal(ctx, f.scope.Org, f.treasuryRow.ID); open {
		t.Fatal("an agent's draft opened a proposal")
	}

	// A member proposes it, as written.
	view, _, err := f.svc.ProposeDraft(ctx, f.scope, d.ID, f.treasuryRow.ID, f.member)
	if err != nil {
		t.Fatalf("ProposeDraft: %v", err)
	}
	if !strings.Contains(view.Description.Canonical(), "1250.5000001") {
		t.Fatalf("the proposal does not carry the drafted amount:\n%s", view.Description.Canonical())
	}
}

// The shared count is the one that holds across instances: a grant of two calls
// an hour allows two, whichever process asks.
func TestTheSharedRateLimitHoldsAcrossCallers(t *testing.T) {
	f := newProposalFixture(t, "shared limit")
	db := store.New(testPool)
	a, b := sharedLimits{store: db}, sharedLimits{store: db}
	ctx := context.Background()
	now := time.Now()
	org := int64(f.scope.Org)

	allowed := 0
	for i, l := range []sharedLimits{a, b, a, b} {
		ok, err := l.Allow(ctx, org, "payroll", 2, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("two limiters sharing a count of 2 an hour allowed %d", allowed)
	}
	// Another connector, or another org, has its own allowance.
	if ok, _ := a.Allow(ctx, org, "rates", 2, now); !ok {
		t.Fatal("one connector's calls counted against another's")
	}
	// An hour on, the window has moved.
	if ok, _ := b.Allow(ctx, org, "payroll", 2, now.Add(61*time.Minute)); !ok {
		t.Fatal("the window did not move after an hour")
	}
	if ok, _ := a.Allow(ctx, org, "payroll", 0, now); ok {
		t.Fatal("a rate of zero allowed a call")
	}
}
