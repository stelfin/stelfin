package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/chat/chattest"
	"github.com/stelfin/stelfin/web"
)

// testTransports returns a registry carrying one fake transport, plus the fake
// itself so a test can inspect what was delivered.
func testTransports(t *testing.T) (*chat.Registry, *chattest.Fake) {
	t.Helper()
	f := chattest.NewFake(chat.Telegram)
	reg, err := chat.NewRegistry("https://stelfin.example", f)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return reg, f
}

func newServer(t *testing.T, f *fixture, treasury *keypair.Full) *Server {
	t.Helper()
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	transports, _ := testTransports(t)
	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL:           "https://stelfin.example",
		Transports:        transports,
		TreasuryAddress:   treasury.Address(),
		SignFeeBump:       signWith(treasury),
		SignProvision:     signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		// Discard logs so refused-request warnings don't clutter test output.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// mustRegistry returns a registry with one fake transport, for tests that do
// not care which platform a message came from.
func mustRegistry(t *testing.T) *chat.Registry {
	t.Helper()
	reg, _ := testTransports(t)
	return reg
}

func do(t *testing.T, srv *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// TestWebhookRoutesByChannel: the channel is a path segment, so an unknown one
// must be refused by an exact lookup against the registry built at startup —
// before a single byte of the body is read.
func TestWebhookRoutesByChannel(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	body := chattest.Envelope(t)

	authentic := chattest.SignedRequest(t, body)
	if rec := do(t, srv, authentic); rec.Code != http.StatusOK {
		t.Errorf("authenticated delivery status = %d, want 200", rec.Code)
	}

	for _, path := range []string{"/webhook/discord", "/webhook/whatsapp", "/webhook/../etc", "/webhook/"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set(chattest.FakeAuthHeader, chattest.FakeSecret)
		if rec := do(t, srv, req); rec.Code == http.StatusOK {
			t.Errorf("%s: status = %d, want a refusal for an unregistered channel", path, rec.Code)
		}
	}
}

// TestWebhookRefusesUnauthenticatedDeliveries: verification runs before parsing
// and before any work, and a caller that fails it learns only that it failed.
func TestWebhookRefusesUnauthenticatedDeliveries(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())
	body := chattest.Envelope(t)

	unauthenticated := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewReader(body))
	rec := do(t, srv, unauthenticated)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "header") {
		t.Errorf("the refusal names which check failed: %q", rec.Body.String())
	}

	wrongSecret := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewReader(body))
	wrongSecret.Header.Set(chattest.FakeAuthHeader, "not-the-secret")
	if rec := do(t, srv, wrongSecret); rec.Code != http.StatusForbidden {
		t.Errorf("wrong secret status = %d, want 403", rec.Code)
	}
}

// TestWebhookAcknowledgesAnUnreadableDelivery: it authenticated, so it came
// from the platform. A shape we cannot read is ours to fix, and retrying it
// would not fix it — so acknowledge rather than invite a retry storm.
func TestWebhookAcknowledgesAnUnreadableDelivery(t *testing.T) {
	f := newFixture(t, sendDecoded())
	tokens, enrollTokens := newTokens(t), newEnrollTokens(t)
	treasury := keypair.MustRandom()

	fake := chattest.NewFake(chat.Telegram)
	fake.FailParse = true
	reg, err := chat.NewRegistry("https://stelfin.example", fake)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL: "https://stelfin.example", Transports: reg,
		TreasuryAddress: treasury.Address(), SignFeeBump: signWith(treasury),
		SignProvision:     signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := do(t, srv, chattest.SignedRequest(t, chattest.Envelope(t)))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a retry cannot fix a shape we cannot read", rec.Code)
	}
}

// TestWebhookWritesThePlatformsAck: the acknowledgement comes from the
// transport, not from this package. Discord answers an interaction with a JSON
// body it will reject if it is wrong, and a bare 200 would fail the
// three-second interaction deadline in a way nothing here would notice.
func TestWebhookWritesThePlatformsAck(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	rec := do(t, srv, chattest.SignedRequest(t, chattest.Envelope(t)))
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("ack body = %q, want the transport's own", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("ack content type = %q", got)
	}
}

func authed(t *testing.T, srv *Server, method, path string, scope Scope, hash, body string) *http.Request {
	t.Helper()
	token, err := srv.tokens.Issue(scope, hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestConfirmEndpoint(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.scope, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.scope, c.Hash, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["amount"] != "5,000.00" {
		t.Errorf("amount = %v, want 5,000.00", got["amount"])
	}
	if got["to_address"] != f.toAddr {
		t.Errorf("to_address = %v, want %s", got["to_address"], f.toAddr)
	}
	if got["xdr"] != c.XDR {
		t.Error("returned envelope differs from the one issued")
	}
}

// TestConfirmIsScopedToTheTokensTransaction: a token names one hash, so it
// cannot be pointed at another payment even for the same user.
func TestConfirmIsScopedToTheTokensTransaction(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.scope, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	// A token for a different (nonexistent) transaction, same user.
	other := strings.Repeat("b", 64)
	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.scope, other, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	_ = c
}

// TestConfirmHidesOtherUsersSends: not-found and not-yours must look the same,
// or the response tells a stranger that a transaction exists.
func TestConfirmHidesOtherUsersSends(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.scope, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	real := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", Scope{Org: f.org, OwnerRef: "mallory"}, c.Hash, ""))
	fake := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", Scope{Org: f.org, OwnerRef: "mallory"}, strings.Repeat("c", 64), ""))

	if real.Code != http.StatusNotFound || fake.Code != http.StatusNotFound {
		t.Fatalf("statuses = %d and %d, want both 404", real.Code, fake.Code)
	}
	if real.Body.String() != fake.Body.String() {
		t.Errorf("a real hash and a fake one produce different responses (%q vs %q); "+
			"the difference tells a stranger the transaction exists",
			real.Body.String(), fake.Body.String())
	}
}

func TestEndpointsRequireAToken(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	for _, path := range []string{"/v1/confirm", "/v1/submit", "/v1/enroll", "/v1/enroll/submit"} {
		method := http.MethodGet
		if path != "/v1/confirm" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: status = %d, want 401", path, rec.Code)
		}

		bad := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		bad.Header.Set("Authorization", "Bearer not-a-token")
		if rec := do(t, srv, bad); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a bad token: status = %d, want 401", path, rec.Code)
		}
	}
}

// TestEnrollEndpointsRejectAConfirmToken and its mirror guard the reason
// EnrollTokens is a distinct type: a token minted for one purpose must not
// open the other, even where the HTTP layer's Bearer-header handling is
// identical.
func TestEnrollEndpointsRejectAConfirmToken(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	confirmToken, err := srv.tokens.Issue(f.scope, strings.Repeat("a", 64), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(`{"address":"`+f.toAddr+`"}`))
	req.Header.Set("Authorization", "Bearer "+confirmToken)
	if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: a confirm token must not authorise enrollment", rec.Code)
	}
}

func TestConfirmEndpointRejectsAnEnrollToken(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.scope, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}
	enrollToken, err := srv.enrollTokens.Issue(f.scope, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/confirm", nil)
	req.Header.Set("Authorization", "Bearer "+enrollToken)
	if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: an enroll token must not authorise reading a payment", rec.Code)
	}
	_ = c
}

func TestSubmitEndpoint(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	srv := newServer(t, f, treasury)
	c, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	body, err := json.Marshal(submitRequest{SignedXDR: signedXDR})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.scope, c.Hash, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["ledger"] != float64(12) {
		t.Errorf("ledger = %v, want 12", got["ledger"])
	}
}

func TestSubmitEndpointRejectsReplay(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	srv := newServer(t, f, treasury)
	c, signedXDR := issueAndSign(t, f, keypair.MustRandom())

	body, _ := json.Marshal(submitRequest{SignedXDR: signedXDR})
	if rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.scope, c.Hash, string(body))); rec.Code != http.StatusOK {
		t.Fatalf("first submit: status = %d", rec.Code)
	}
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.scope, c.Hash, string(body)))
	if rec.Code != http.StatusConflict {
		t.Errorf("replay status = %d, want 409", rec.Code)
	}
}

func TestSubmitEndpointRejectsForeignTransaction(t *testing.T) {
	f := newFixture(t, sendDecoded())
	treasury := keypair.MustRandom()
	srv := newServer(t, f, treasury)

	attacker := keypair.MustRandom()
	foreign, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: attacker.Address(), Sequence: 1},
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 100}},
		BaseFee:              1000,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	signed, _ := foreign.Sign(network.TestNetworkPassphrase, attacker)
	xdr, _ := signed.Base64()
	body, _ := json.Marshal(submitRequest{SignedXDR: xdr})

	hash, _ := signed.HashHex(network.TestNetworkPassphrase)
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.scope, hash, string(body)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the treasury must not fee-bump a transaction we never issued", rec.Code)
	}
}

// TestConfirmLinkPutsTokenInTheFragment: query strings land in access logs,
// proxy logs, and Referer headers. Fragments do not reach the server at all.
func TestConfirmLinkPutsTokenInTheFragment(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	link, err := srv.IssueConfirmLink(f.scope, strings.Repeat("a", 64), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueConfirmLink: %v", err)
	}
	if !strings.Contains(link, "#") {
		t.Fatalf("link %q has no fragment", link)
	}
	if strings.Contains(link, "?") {
		t.Errorf("link %q carries a query string; the token would reach server logs", link)
	}
}

// TestEnrollLinkPutsTokenInTheFragment mirrors TestConfirmLinkPutsTokenInTheFragment.
func TestEnrollLinkPutsTokenInTheFragment(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	link, err := srv.IssueEnrollLink(f.scope, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueEnrollLink: %v", err)
	}
	if !strings.Contains(link, "/enroll#") {
		t.Fatalf("link %q does not carry the enroll fragment", link)
	}
	if strings.Contains(link, "?") {
		t.Errorf("link %q carries a query string; the token would reach server logs", link)
	}
}

func authedEnroll(t *testing.T, srv *Server, method, path string, scope Scope, body string) *http.Request {
	t.Helper()
	token, err := srv.enrollTokens.Issue(scope, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestEnrollEndpoint(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	srv, err := NewServer(svc, tokens, enrollTokens, ServerConfig{
		BaseURL: "https://stelfin.example", Transports: mustRegistry(t),
		TreasuryAddress: treasury.Address(), SignFeeBump: signWith(treasury), SignProvision: signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	userKey := keypair.MustRandom()
	body, err := json.Marshal(enrollRequest{Address: userKey.Address()})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	rec := do(t, srv, authedEnroll(t, srv, http.MethodPost, "/v1/enroll", owner, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["address"] != userKey.Address() {
		t.Errorf("address = %v, want %s", got["address"], userKey.Address())
	}
	if got["xdr"] == "" || got["xdr"] == nil {
		t.Error("response carries no transaction")
	}
}

func TestEnrollSubmitEndpoint(t *testing.T) {
	svc, owner := newUnenrolledService(t)
	treasury := keypair.MustRandom()
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	srv, err := NewServer(svc, tokens, enrollTokens, ServerConfig{
		BaseURL: "https://stelfin.example", Transports: mustRegistry(t),
		TreasuryAddress: treasury.Address(), SignFeeBump: signWith(treasury), SignProvision: signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	userKey := keypair.MustRandom()
	_, signedXDR := prepareAndSign(t, svc, owner, treasury.Address(), userKey)

	body, err := json.Marshal(enrollSubmitRequest{SignedXDR: signedXDR})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	rec := do(t, srv, authedEnroll(t, srv, http.MethodPost, "/v1/enroll/submit", owner, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["ledger"] != float64(7) {
		t.Errorf("ledger = %v, want 7", got["ledger"])
	}
	if got["address"] != userKey.Address() {
		t.Errorf("address = %v, want %s", got["address"], userKey.Address())
	}
}

func TestNewServerValidatesConfig(t *testing.T) {
	f := newFixture(t, sendDecoded())
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	treasury := keypair.MustRandom()

	full := ServerConfig{
		BaseURL:           "https://stelfin.example",
		Transports:        mustRegistry(t),
		NetworkPassphrase: network.TestNetworkPassphrase,
		TreasuryAddress:   treasury.Address(),
		SignFeeBump:       signWith(treasury),
		SignProvision:     signProvisionWith(treasury),
	}
	for name, mutate := range map[string]func(*ServerConfig){
		"no transports":       func(c *ServerConfig) { c.Transports = nil },
		"no base url":         func(c *ServerConfig) { c.BaseURL = "" },
		"no network":          func(c *ServerConfig) { c.NetworkPassphrase = "" },
		"no treasury":         func(c *ServerConfig) { c.TreasuryAddress = "" },
		"no fee-bump signer":  func(c *ServerConfig) { c.SignFeeBump = nil },
		"no provision signer": func(c *ServerConfig) { c.SignProvision = nil },
	} {
		cfg := full
		mutate(&cfg)
		if _, err := NewServer(f.svc, tokens, enrollTokens, cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := NewServer(f.svc, nil, enrollTokens, full); err == nil {
		t.Error("expected an error when confirm tokens are missing")
	}
	if _, err := NewServer(f.svc, tokens, nil, full); err == nil {
		t.Error("expected an error when enroll tokens are missing")
	}
}

func TestConfirmPageIsServedWithAStrictPolicy(t *testing.T) {
	f := newFixture(t, sendDecoded())
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	treasury := keypair.MustRandom()

	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL:           "https://stelfin.example",
		Transports:        mustRegistry(t),
		TreasuryAddress:   treasury.Address(),
		SignFeeBump:       signWith(treasury),
		SignProvision:     signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Assets:            web.Handler(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/confirm", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Confirm this payment") {
		t.Error("the confirmation page was not served")
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no content security policy")
	}
	// An inline script would force the policy open and remove most of its
	// value, which is why the page's script lives in its own file.
	if strings.Contains(csp, "script-src") && strings.Contains(csp, "'unsafe-inline'") {
		before, _, _ := strings.Cut(csp, "style-src")
		if strings.Contains(before, "'unsafe-inline'") {
			t.Errorf("script-src allows inline script: %s", csp)
		}
	}
	// A script that ran despite the policy still must not be able to send a
	// signing key to another host.
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("connect-src is not restricted to the origin: %s", csp)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer: the token rides in the fragment", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestRootRedirectsToMarketingSite: the binary's own "/" has no landing page
// of its own any more — the marketing site is a separate Next.js app — so a
// stray visitor should land there rather than see a 404 or a stale duplicate.
func TestRootRedirectsToMarketingSite(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != marketingURL {
		t.Errorf("Location = %q, want %q", got, marketingURL)
	}

	// "/{$}" matches only the exact root path — confirm an unmatched path
	// still 404s rather than being swallowed by the redirect.
	miss := do(t, srv, httptest.NewRequest(http.MethodGet, "/this-does-not-exist", nil))
	if miss.Code != http.StatusNotFound {
		t.Errorf("unmatched path status = %d, want 404", miss.Code)
	}
}

// TestStaticAssetsAreServed pins the bug every page-serving test above missed:
// they check that /confirm and /enroll return 200 and mention the right text,
// but never that the <script src="/static/..."> files those pages actually
// reference resolve. Static() returns a filesystem already rooted at
// static/, so the /static/ prefix has to be stripped before reaching it —
// without that, every script tag 404s and neither confirm.js nor enroll.js
// ever runs in a real browser, silently, since the HTML page itself still
// serves fine either way.
func TestStaticAssetsAreServed(t *testing.T) {
	f := newFixture(t, sendDecoded())
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	treasury := keypair.MustRandom()

	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL: "https://stelfin.example", Transports: mustRegistry(t),
		TreasuryAddress: treasury.Address(), SignFeeBump: signWith(treasury), SignProvision: signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Assets:            web.Handler(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	for _, path := range []string{"/static/confirm.js", "/static/enroll.js", "/static/stellar-sdk.min.js"} {
		rec := do(t, srv, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

// TestConfirmResponseCarriesTheNetwork: the page parses the envelope itself and
// must do it against the same network the server signed for.
func TestConfirmResponseCarriesTheNetwork(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.scope, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}
	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.scope, c.Hash, ""))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["network_passphrase"] != network.TestNetworkPassphrase {
		t.Errorf("network_passphrase = %v, want the test network", got["network_passphrase"])
	}
}
