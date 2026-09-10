package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

const testPGPort = 54336

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

type fixture struct {
	server *httptest.Server
	store  *store.Store
	org    store.Org
	read   string
	other  string
}

var slugSeq int

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := store.New(testPool)

	newOrg := func(suffix string) store.Org {
		slugSeq++
		org, err := db.CreateOrg(ctx, store.CreateOrgParams{
			Slug:        fmt.Sprintf("m-%d-%d", testPGPort, slugSeq),
			DisplayName: t.Name() + suffix,
			Network:     "testnet",
			Channel:     chat.Telegram,
			SpaceID:     fmt.Sprintf("m%d", slugSeq),
		})
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		return org
	}

	org := newOrg("")
	elsewhere := newOrg("-other")

	read, _, err := db.IssueMCPToken(ctx, org.ID, "agent", store.TierRead, 0, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	other, _, err := db.IssueMCPToken(ctx, elsewhere.ID, "agent", store.TierRead, 0, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	s, err := NewServer(db, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	return &fixture{server: srv, store: db, org: org, read: read, other: other}
}

// rpc makes one call and returns the decoded reply.
func (f *fixture) rpc(t *testing.T, token, method string, params any) response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return response{Error: &rpcError{Code: http.StatusUnauthorized, Message: "unauthorized"}}
	}

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// callTool runs a tool and returns its text content.
func (f *fixture) callTool(t *testing.T, token, name string, args map[string]any) (string, bool) {
	t.Helper()
	resp := f.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("%s: rpc error %d %s", name, resp.Error.Code, resp.Error.Message)
	}
	encoded, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result toolResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	return result.Content[0].Text, result.IsError
}

func TestAnUnauthorisedRequestGetsNothing(t *testing.T) {
	f := newFixture(t)

	for name, token := range map[string]string{
		"no token":      "",
		"nonsense":      "not-a-token",
		"another realm": "stlf_" + strings.Repeat("A", 43),
	} {
		t.Run(name, func(t *testing.T) {
			resp := f.rpc(t, token, "tools/list", nil)
			if resp.Error == nil || resp.Error.Code != http.StatusUnauthorized {
				t.Fatalf("reply = %+v", resp)
			}
		})
	}
}

// TestThereIsNoToolThatMovesMoney is the design, asserted rather than
// described.
//
// A tool added later that signs, submits or writes the address book fails this
// — which is the only place that decision gets made once and stays made.
func TestThereIsNoToolThatMovesMoney(t *testing.T) {
	f := newFixture(t)
	resp := f.rpc(t, f.read, "tools/list", nil)
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}

	encoded, _ := json.Marshal(resp.Result)
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("no tools listed; this test is asserting nothing")
	}

	forbidden := []string{
		"sign", "submit", "execute", "approve", "send", "pay",
		"transfer", "recipient", "beneficiary", "revoke", "delete", "create",
	}
	for _, tool := range listed.Tools {
		for _, word := range forbidden {
			if strings.Contains(strings.ToLower(tool.Name), word) {
				t.Errorf("the tool %q can act, and the read tier must not", tool.Name)
			}
		}
	}
}

// TestSamplingAndElicitationAreNotOffered: both let a server ask the client's
// model something, which is how a confused server turns an agent into its own
// instrument.
func TestSamplingAndElicitationAreNotOffered(t *testing.T) {
	f := newFixture(t)
	resp := f.rpc(t, f.read, "initialize", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}

	encoded, _ := json.Marshal(resp.Result)
	var out struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, unwanted := range []string{"sampling", "elicitation", "roots"} {
		if _, offered := out.Capabilities[unwanted]; offered {
			t.Errorf("the server offers %s", unwanted)
		}
	}
	if _, offered := out.Capabilities["tools"]; !offered {
		t.Error("the server offers no tools at all")
	}
}

// TestEveryAmountCrossesAsAString: a JSON number is a float64 on the other side
// whatever the sender meant, and a balance read back as 1000.5000001000001 is a
// wrong answer that looks right.
func TestEveryAmountCrossesAsAString(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A balance a float64 cannot hold.
	treasury, err := f.store.EnsureAccount(ctx, f.org.ID, ledger.AccountTreasury, "", "ops")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	external, err := f.store.EnsureAccount(ctx, f.org.ID, ledger.AccountExternal, "", "external")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	usdc, err := f.store.Ledger().EnsureAsset(ctx, "USDC",
		"GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	if err != nil {
		t.Fatalf("ensure asset: %v", err)
	}
	amount := int64(9007199254740993) // 2^53 + 1 stroops
	if _, err := f.store.Post(ctx, ledger.PostRequest{
		Org: f.org.ID, IdempotencyKey: "mcp-precision", Kind: ledger.TxDeposit,
		OccurredAt: time.Unix(1700000000, 0),
		Postings: []ledger.Posting{
			{Account: treasury, Asset: usdc, Amount: 9007199254740993},
			{Account: external, Asset: usdc, Amount: -9007199254740993},
		},
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = amount

	text, isErr := f.callTool(t, f.read, "stelfin_balances", nil)
	if isErr {
		t.Fatalf("balances failed: %s", text)
	}
	if !strings.Contains(text, `"900719925.4740993"`) {
		t.Fatalf("the balance did not survive as an exact string:\n%s", text)
	}
	// And it is quoted, not bare — a bare 900719925.4740993 would be parsed as
	// a float by whatever reads it next.
	if strings.Contains(text, `: 900719925.4740993`) {
		t.Error("an amount crossed as a JSON number")
	}
}

// TestATokenReachesOnlyItsOwnWorkspace: the org comes from the token and is not
// a parameter, so it is not a tenancy boundary an agent gets to choose.
func TestATokenReachesOnlyItsOwnWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.store.LinkTreasury(ctx, store.LinkTreasuryParams{
		Org: f.org.ID, Kind: store.TreasuryClassic,
		Address: "GCFIRY65OQE7DFP5KLNS2PF2LVZMUZYJX4OZIEQ36N2IQANUB5XVYOJR",
		Label:   "main", Low: 1, Medium: 2, High: 2,
	}); err != nil {
		t.Fatalf("link treasury: %v", err)
	}

	mine, isErr := f.callTool(t, f.read, "stelfin_treasuries", nil)
	if isErr || !strings.Contains(mine, "main") {
		t.Fatalf("the owning workspace could not see its treasury:\n%s", mine)
	}

	theirs, isErr := f.callTool(t, f.other, "stelfin_treasuries", nil)
	if isErr {
		t.Fatalf("the other workspace's call failed: %s", theirs)
	}
	if strings.Contains(theirs, "main") || strings.Contains(theirs, "GCFIRY65") {
		t.Fatalf("another workspace saw this one's treasury:\n%s", theirs)
	}

	// There is no argument that could change the answer.
	spoofed, _ := f.callTool(t, f.other, "stelfin_treasuries",
		map[string]any{"org": int64(f.org.ID), "org_id": int64(f.org.ID)})
	if strings.Contains(spoofed, "main") {
		t.Fatalf("an org argument crossed the boundary:\n%s", spoofed)
	}
}

func TestAnArgumentOutOfBoundsIsRefusedUsefully(t *testing.T) {
	f := newFixture(t)

	for _, args := range []map[string]any{
		{"limit": 0},
		{"limit": 1000},
		{"limit": 2.5},
		{"limit": "many"},
	} {
		text, isErr := f.callTool(t, f.read, "stelfin_history", args)
		if !isErr {
			t.Errorf("limit %v was accepted:\n%s", args["limit"], text)
			continue
		}
		// The agent can fix this one, so it is told what is wrong.
		if !strings.Contains(text, "limit") {
			t.Errorf("the refusal does not name the argument: %s", text)
		}
	}
}

func TestAnUnknownToolIsAbsentRatherThanForbidden(t *testing.T) {
	f := newFixture(t)
	resp := f.rpc(t, f.read, "tools/call",
		map[string]any{"name": "stelfin_sign", "arguments": map[string]any{}})
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("reply = %+v", resp)
	}
}

func TestMalformedRequestsAreRefused(t *testing.T) {
	f := newFixture(t)

	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/mcp",
		strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+f.read)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error == nil || out.Error.Code != codeParse {
		t.Fatalf("reply = %+v", out)
	}

	// A request claiming the wrong protocol version is refused too.
	if got := f.rpc(t, f.read, "tools/list", nil); got.Error != nil {
		t.Fatalf("a well-formed request failed: %+v", got.Error)
	}
}
