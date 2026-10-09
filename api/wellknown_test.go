package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
)

// The file says which key signs this deployment's challenges, which is the one
// thing anybody fetching it is there to find out, and nothing it cannot back.
func TestStellarTOMLNamesTheKeyThatSignsChallenges(t *testing.T) {
	f := newDaoFixture(t, "toml")
	srv := callServer(t, f)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/stellar.toml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET stellar.toml = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`SIGNING_KEY = "` + f.svc.Challenges().ServerAccount() + `"`,
		`NETWORK_PASSPHRASE = "` + network.TestNetworkPassphrase + `"`,
		`ACCOUNTS = ["` + f.treasuryFixture.treasury.Address() + `"]`,
		`ORG_NAME = "stelfin"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the file lacks %s:\n%s", want, body)
		}
	}
	// It issues nothing and is no anchor, and does not say otherwise.
	for _, absent := range []string{"[[CURRENCIES]]", "TRANSFER_SERVER", "WEB_AUTH_ENDPOINT", "KYC_SERVER"} {
		if strings.Contains(body, absent) {
			t.Errorf("the file claims %s", absent)
		}
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q; a wallet on another origin could not read it", got)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}

	// Without a key to name there is nothing to say.
	f.svc.challenges = nil
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/stellar.toml", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("with no web-auth key: %d, want 404", rec.Code)
	}
}
