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

	"github.com/stelfin/stelfin/web"
)

const testVerifyToken = "verify-me"

func newServer(t *testing.T, f *fixture, treasury *keypair.Full) *Server {
	t.Helper()
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL:           "https://stelfin.example",
		Messenger:         &fakeMessenger{},
		AppSecret:         testSecret,
		VerifyToken:       testVerifyToken,
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

func do(t *testing.T, srv *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestChallengeEndpoint(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	req := httptest.NewRequest(http.MethodGet,
		"/webhook/whatsapp?hub.mode=subscribe&hub.verify_token="+testVerifyToken+"&hub.challenge=42", nil)
	rec := do(t, srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "42" {
		t.Errorf("body = %q, want %q", got, "42")
	}
}

func TestChallengeRejectsWrongToken(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	req := httptest.NewRequest(http.MethodGet,
		"/webhook/whatsapp?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=42", nil)
	if rec := do(t, srv, req); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestInboundRequiresValidSignature(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())
	body := []byte(`{"entry":[]}`)

	signed := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", bytes.NewReader(body))
	signed.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	if rec := do(t, srv, signed); rec.Code != http.StatusOK {
		t.Errorf("signed delivery status = %d, want 200", rec.Code)
	}

	unsigned := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", bytes.NewReader(body))
	if rec := do(t, srv, unsigned); rec.Code != http.StatusForbidden {
		t.Errorf("unsigned delivery status = %d, want 403", rec.Code)
	}

	forged := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", bytes.NewReader(body))
	forged.Header.Set("X-Hub-Signature-256", sign([]byte("ffffffffffffffffffffffffffffffff"), body))
	if rec := do(t, srv, forged); rec.Code != http.StatusForbidden {
		t.Errorf("forged delivery status = %d, want 403", rec.Code)
	}
}

func authed(t *testing.T, srv *Server, method, path, owner, hash, body string) *http.Request {
	t.Helper()
	token, err := srv.tokens.Issue(owner, hash, time.Now().Add(time.Hour))
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

	c, err := f.svc.PrepareSend(t.Context(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.owner, c.Hash, ""))
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

	c, err := f.svc.PrepareSend(t.Context(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	// A token for a different (nonexistent) transaction, same user.
	other := strings.Repeat("b", 64)
	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.owner, other, ""))
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

	c, err := f.svc.PrepareSend(t.Context(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}

	real := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", "mallory", c.Hash, ""))
	fake := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", "mallory", strings.Repeat("c", 64), ""))

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

	confirmToken, err := srv.tokens.Issue(f.owner, strings.Repeat("a", 64), time.Now().Add(time.Hour))
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

	c, err := f.svc.PrepareSend(t.Context(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}
	enrollToken, err := srv.enrollTokens.Issue(f.owner, time.Now().Add(time.Hour))
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
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.owner, c.Hash, string(body)))
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
	if rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.owner, c.Hash, string(body))); rec.Code != http.StatusOK {
		t.Fatalf("first submit: status = %d", rec.Code)
	}
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.owner, c.Hash, string(body)))
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
	rec := do(t, srv, authed(t, srv, http.MethodPost, "/v1/submit", f.owner, hash, string(body)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the treasury must not fee-bump a transaction we never issued", rec.Code)
	}
}

// TestConfirmLinkPutsTokenInTheFragment: query strings land in access logs,
// proxy logs, and Referer headers. Fragments do not reach the server at all.
func TestConfirmLinkPutsTokenInTheFragment(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	link, err := srv.IssueConfirmLink(f.owner, strings.Repeat("a", 64), time.Now().Add(time.Hour))
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

	link, err := srv.IssueEnrollLink("+2348012345678", time.Now().Add(time.Hour))
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

func authedEnroll(t *testing.T, srv *Server, method, path, owner, body string) *http.Request {
	t.Helper()
	token, err := srv.enrollTokens.Issue(owner, time.Now().Add(time.Hour))
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
		BaseURL: "https://stelfin.example", Messenger: &fakeMessenger{},
		AppSecret: testSecret, VerifyToken: testVerifyToken,
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
		BaseURL: "https://stelfin.example", Messenger: &fakeMessenger{},
		AppSecret: testSecret, VerifyToken: testVerifyToken,
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
		AppSecret:       testSecret,
		VerifyToken:     testVerifyToken,
		TreasuryAddress: treasury.Address(),
		SignFeeBump:     signWith(treasury),
		SignProvision:   signProvisionWith(treasury),
	}
	for name, mutate := range map[string]func(*ServerConfig){
		"no app secret":       func(c *ServerConfig) { c.AppSecret = nil },
		"no verify token":     func(c *ServerConfig) { c.VerifyToken = "" },
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
		Messenger:         &fakeMessenger{},
		AppSecret:         testSecret,
		VerifyToken:       testVerifyToken,
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

// TestLandingPageIsServedAtRoot: unlike /confirm and /enroll, the root path
// carries no token and no authority — it's the one page a stranger is meant
// to land on directly.
func TestLandingPageIsServedAtRoot(t *testing.T) {
	f := newFixture(t, sendDecoded())
	tokens := newTokens(t)
	enrollTokens := newEnrollTokens(t)
	treasury := keypair.MustRandom()

	srv, err := NewServer(f.svc, tokens, enrollTokens, ServerConfig{
		BaseURL: "https://stelfin.example", Messenger: &fakeMessenger{},
		AppSecret: testSecret, VerifyToken: testVerifyToken,
		TreasuryAddress: treasury.Address(), SignFeeBump: signWith(treasury), SignProvision: signProvisionWith(treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Assets:            web.Handler(),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "stelfin") {
		t.Error("the landing page was not served")
	}

	// An unmatched path must still 404, not fall through to something
	// unexpected now that "/" is a catch-all pattern.
	miss := do(t, srv, httptest.NewRequest(http.MethodGet, "/this-does-not-exist", nil))
	if miss.Code != http.StatusNotFound {
		t.Errorf("unmatched path status = %d, want 404", miss.Code)
	}
}

// TestConfirmResponseCarriesTheNetwork: the page parses the envelope itself and
// must do it against the same network the server signed for.
func TestConfirmResponseCarriesTheNetwork(t *testing.T) {
	f := newFixture(t, sendDecoded())
	srv := newServer(t, f, keypair.MustRandom())

	c, err := f.svc.PrepareSend(t.Context(), f.owner, []string{sendMessage})
	if err != nil {
		t.Fatalf("PrepareSend: %v", err)
	}
	rec := do(t, srv, authed(t, srv, http.MethodGet, "/v1/confirm", f.owner, c.Hash, ""))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["network_passphrase"] != network.TestNetworkPassphrase {
		t.Errorf("network_passphrase = %v, want the test network", got["network_passphrase"])
	}
}
