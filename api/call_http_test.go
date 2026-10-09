package api

import (
	"bytes"
	"context"
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
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/contracts/treasuryabi"
)

// callServer is the HTTP surface over a workspace with a contract treasury.
func callServer(t *testing.T, f *daoFixture) *Server {
	t.Helper()
	transports, _ := testTransports(t)
	srv, err := NewServer(f.svc, newTokens(t), newEnrollTokens(t), newLinkTokens(t), ServerConfig{
		BaseURL:           "https://stelfin.example",
		Transports:        transports,
		Handler:           &stubHandler{},
		TreasuryAddress:   f.treasuryFixture.treasury.Address(),
		SignFeeBump:       signWith(f.treasuryFixture.treasury),
		SignProvision:     signProvisionWith(f.treasuryFixture.treasury),
		NetworkPassphrase: network.TestNetworkPassphrase,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// tokenFrom pulls the token out of a link the server minted.
func tokenFrom(t *testing.T, link string) string {
	t.Helper()
	_, token, ok := strings.Cut(link, "#")
	if !ok {
		t.Fatalf("no token in %q", link)
	}
	return token
}

func callEndpoint(t *testing.T, srv *Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// What the page is given is the envelope, the description to check it
// against, who and what the link was made for, and stelfin's own word set
// apart — and a link is good for the one call it names.
func TestTheCallPageIsGivenWhatItChecksAndWhatItCannot(t *testing.T) {
	f := newDaoFixture(t, "call http")
	ctx := context.Background()
	srv := callServer(t, f)
	to := keypair.MustRandom().Address()

	one, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, payTo(t, f, to), "March invoice")
	if err != nil {
		t.Fatal(err)
	}
	link, err := srv.IssueCallLink(f.scope, one.Hash, one.ExpiresAt)
	if err != nil || !strings.HasPrefix(link, "https://stelfin.example/call#") {
		t.Fatalf("link %q, %v", link, err)
	}
	token := tokenFrom(t, link)

	code, body := callEndpoint(t, srv, http.MethodGet, "/v1/call", token, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/call = %d", code)
	}
	for field, want := range map[string]any{
		"hash":               one.Hash,
		"treasury":           f.treasury.Address,
		"treasury_label":     "ops",
		"member":             f.wallet.Address(),
		"note":               "March invoice",
		"network_passphrase": network.TestNetworkPassphrase,
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %v", field, body[field], want)
		}
	}
	if says, _ := body["says"].(string); !strings.Contains(says, "pay 250.00 XLM to "+to) {
		t.Errorf("says %q", body["says"])
	}
	if canonical, _ := body["canonical"].(string); !strings.Contains(canonical, "propose") {
		t.Errorf("the canonical description does not name the function: %q", canonical)
	}
	assets, _ := body["assets"].([]any)
	if len(assets) != 2 {
		t.Errorf("asset hints: %v", body["assets"])
	}

	// No token, and a token for something else.
	if code, _ := callEndpoint(t, srv, http.MethodGet, "/v1/call", "", nil); code != http.StatusUnauthorized {
		t.Errorf("with no token: %d", code)
	}
	stray, _ := srv.IssueCallLink(f.scope, strings.Repeat("ab", 32), time.Now().Add(time.Minute))
	if code, _ := callEndpoint(t, srv, http.MethodGet, "/v1/call", tokenFrom(t, stray), nil); code != http.StatusNotFound {
		t.Errorf("a token naming no call: %d", code)
	}
	// A link to confirm a payment is the same kind of token and names a hash
	// too. It finds no call, because a hash names one or the other.
	if code, _ := callEndpoint(t, srv, http.MethodGet, "/v1/confirm", token, nil); code != http.StatusNotFound {
		t.Errorf("a call's token at the payment page: %d", code)
	}
}

// A link carries one call in. Not another, even another of the same member's
// own; and each way of being turned away has its own answer for the page.
func TestACallLinkSendsTheCallItNamesAndNoOther(t *testing.T) {
	f := newDaoFixture(t, "call submit http")
	ctx := context.Background()
	srv := callServer(t, f)
	to := keypair.MustRandom().Address()

	one, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, payTo(t, f, to), "one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, payTo(t, f, to), "two")
	if err != nil {
		t.Fatal(err)
	}
	link, _ := srv.IssueCallLink(f.scope, one.Hash, one.ExpiresAt)
	token := tokenFrom(t, link)
	first, _ := f.svc.LoadMemberCall(ctx, f.scope, one.Hash)
	second, _ := f.svc.LoadMemberCall(ctx, f.scope, two.Hash)

	submit := func(signedXDR string) (int, map[string]any) {
		return callEndpoint(t, srv, http.MethodPost, "/v1/call/submit", token,
			map[string]string{"signed_xdr": signedXDR})
	}

	if code, _ := submit(""); code != http.StatusBadRequest {
		t.Errorf("nothing at all: %d", code)
	}
	// Her other call, properly signed, through this call's link.
	if code, _ := submit(signEnvelope(t, second.Call.XDR, f.wallet)); code != http.StatusBadRequest {
		t.Errorf("another of her calls through this link: %d", code)
	}
	if len(f.chain.sent) != 0 {
		t.Fatal("something was sent through a link that does not name it")
	}
	if code, _ := submit(first.Call.XDR); code != http.StatusBadRequest {
		t.Errorf("unsigned: %d", code)
	}

	// Turned away by the network before a ledger: the signature's fault, and
	// the page may try again.
	f.chain.sendStatus = stellarcore.TXStatusError
	if code, _ := submit(signEnvelope(t, first.Call.XDR, keypair.MustRandom())); code != http.StatusBadRequest {
		t.Errorf("a signature the network refused: %d", code)
	}

	// Sent.
	f.chain.sendStatus = stellarcore.TXStatusPending
	f.chain.landedWith = ptrTo(treasuryabi.U32(7))
	code, body := submit(signEnvelope(t, first.Call.XDR, f.wallet))
	if code != http.StatusOK || body["proposal"] != float64(7) || body["function"] != "propose" || body["hash"] == "" {
		t.Fatalf("POST /v1/call/submit = %d %v", code, body)
	}
	// And only once.
	if code, _ := submit(signEnvelope(t, first.Call.XDR, f.wallet)); code != http.StatusConflict {
		t.Errorf("a second time: %d", code)
	}
	if code, _ := callEndpoint(t, srv, http.MethodGet, "/v1/call", token, nil); code != http.StatusConflict {
		t.Errorf("looking at a call already sent: %d", code)
	}

	// The other call was built for the sequence number the first has now
	// used. Its page is told it is gone, which is a different thing from a
	// signature that was wrong.
	badSeq := xdr.TransactionResultCodeTxBadSeq
	stale, _ := xdr.MarshalBase64(xdr.TransactionResult{Result: xdr.TransactionResultResult{
		Code: xdr.TransactionResultCodeTxFeeBumpInnerFailed,
		InnerResultPair: &xdr.InnerTransactionResultPair{
			Result: xdr.InnerTransactionResult{Result: xdr.InnerTransactionResultResult{Code: badSeq}},
		},
	}})
	f.chain.sendStatus, f.chain.errorResult = stellarcore.TXStatusError, stale
	other, _ := srv.IssueCallLink(f.scope, two.Hash, two.ExpiresAt)
	code, _ = callEndpoint(t, srv, http.MethodPost, "/v1/call/submit", tokenFrom(t, other),
		map[string]string{"signed_xdr": signEnvelope(t, second.Call.XDR, f.wallet)})
	if code != http.StatusGone {
		t.Errorf("a call the wallet has moved past: %d, want 410", code)
	}

	// A call that reached a ledger and was refused there.
	three, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, payTo(t, f, to), "three")
	if err != nil {
		t.Fatal(err)
	}
	third, _ := f.svc.LoadMemberCall(ctx, f.scope, three.Hash)
	f.chain.sendStatus, f.chain.errorResult, f.chain.failed = stellarcore.TXStatusPending, "", true
	last, _ := srv.IssueCallLink(f.scope, three.Hash, three.ExpiresAt)
	code, _ = callEndpoint(t, srv, http.MethodPost, "/v1/call/submit", tokenFrom(t, last),
		map[string]string{"signed_xdr": signEnvelope(t, third.Call.XDR, f.wallet)})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("a call the contract refused on chain: %d, want 422", code)
	}
}
