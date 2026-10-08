package sheets

// These tests live inside the package, unlike the reader's, for one reason:
// the option that points the exchange away from Google is unexported so that
// nothing but a test can use it, and a test has to be in here to be that test.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stelfin/stelfin/connector"
)

// testKey is one 2048-bit key for the whole file, because generating one per
// test is most of a second each and proves nothing more.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// pkcs8 renders a key the way Google writes one into a key file.
func pkcs8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// keyFile builds a service-account JSON around the test key, with whatever
// fields a test wants changed. A nil value removes the field.
func keyFile(t *testing.T, changes map[string]any) []byte {
	t.Helper()
	fields := map[string]any{
		"type":           "service_account",
		"project_id":     "stelfin-test",
		"private_key_id": "key-id-1",
		"private_key":    pkcs8(t, testKey()),
		"client_email":   "reader@stelfin-test.iam.gserviceaccount.com",
		"client_id":      "100000000000000000000",
		"token_uri":      "https://oauth2.googleapis.com/token",
	}
	for name, value := range changes {
		if value == nil {
			delete(fields, name)
			continue
		}
		fields[name] = value
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// clock is time a test moves by hand. It also counts how often it is read,
// which is how a test knows a caller has got as far as looking at the cache.
type clock struct {
	mu    sync.Mutex
	at    time.Time
	reads int
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.at
}

func (c *clock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(by)
}

// awaitReads spins until the clock has been read that many times. It yields
// rather than sleeps, and gives up loudly rather than hang the suite.
func (c *clock) awaitReads(t *testing.T, reads int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		got := c.reads
		c.mu.Unlock()
		if got >= reads {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clock was read %d times, and %d were expected", got, reads)
		}
		runtime.Gosched()
	}
}

// tokenServer is a stand-in for Google's token endpoint that keeps what it was
// sent.
type tokenServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
}

// serveTokens stands up a token endpoint. respond is given the number of the
// request, counting from one.
func serveTokens(
	t *testing.T, respond func(n int, w http.ResponseWriter, r *http.Request),
) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.requests = append(ts.requests, r)
		n := len(ts.requests)
		ts.mu.Unlock()
		respond(n, w, r)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) url() string { return ts.srv.URL + "/token" }

func (ts *tokenServer) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.requests)
}

// assertions is every assertion the server has been posted.
func (ts *tokenServer) assertions() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]string, 0, len(ts.requests))
	for _, r := range ts.requests {
		out = append(out, r.PostForm.Get("assertion"))
	}
	return out
}

// grant answers the way Google does.
func grant(w http.ResponseWriter, token string, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"access_token":%q,"expires_in":%d,"token_type":"Bearer"}`, token, expiresIn)
}

// numbered grants a different token on every request, so a test can tell a
// reused token from a fresh one.
func numbered(n int, w http.ResponseWriter, _ *http.Request) {
	grant(w, fmt.Sprintf("ya29.token-%d", n), 3600)
}

// account builds a ServiceAccount pointed at a test server.
func account(t *testing.T, ts *tokenServer, c *clock, options ...Option) *ServiceAccount {
	t.Helper()
	options = append([]Option{withTokenEndpoint(ts.url()), WithClock(c.Now)}, options...)
	a, err := NewServiceAccount(keyFile(t, nil), ts.srv.Client(), options...)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return a
}

// readAssertion takes a JWT apart and verifies its signature, returning the
// header and the claims.
func readAssertion(
	t *testing.T, assertion string, public *rsa.PublicKey,
) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("the assertion has %d parts, and a JWT has three", len(parts))
	}

	decode := func(part string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatalf("a part is not unpadded base64url: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("a part is not JSON: %v", err)
		}
		return out
	}
	header, claims = decode(parts[0]), decode(parts[1])

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the signature is not unpadded base64url: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the signature does not verify under the account's key: %v", err)
	}
	return header, claims
}

// keyFragments is pieces of the private key that must never be seen in an
// error: every line of its PEM body, and the start of each, so that a message
// quoting only part of a line is caught as well.
func keyFragments(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(pkcs8(t, testKey()), "\n") {
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		out = append(out, line)
		if len(line) > 16 {
			out = append(out, line[:16])
		}
	}
	return out
}

// mustNotQuote fails if an error contains any of the secrets.
func mustNotQuote(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Errorf("the error quotes a secret back (%.12s…): %v", secret, err)
		}
	}
}

// TestTheAssertionIsTheOneGoogleDocuments reads what was actually posted.
//
// Every claim is checked against the wire rather than against the code that
// built it, and the signature is verified with the public half of the key, so
// this fails if the assertion would be refused by a real token endpoint.
func TestTheAssertionIsTheOneGoogleDocuments(t *testing.T) {
	ts := serveTokens(t, numbered)
	c := newClock()

	token, err := account(t, ts, c).Token(context.Background(), 42)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if token != "ya29.token-1" {
		t.Errorf("token = %q", token)
	}

	if ts.count() != 1 {
		t.Fatalf("%d requests", ts.count())
	}
	r := ts.requests[0]
	if r.Method != http.MethodPost {
		t.Errorf("method = %s", r.Method)
	}
	if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", got)
	}
	if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("grant_type = %q", got)
	}
	// Two fields and no others: nothing rides along with the assertion.
	if len(r.PostForm) != 2 {
		t.Errorf("the form has %d fields", len(r.PostForm))
	}

	header, claims := readAssertion(t, r.PostForm.Get("assertion"), &testKey().PublicKey)

	for name, want := range map[string]any{"alg": "RS256", "typ": "JWT", "kid": "key-id-1"} {
		if header[name] != want {
			t.Errorf("header %s = %v, want %v", name, header[name], want)
		}
	}
	if len(header) != 3 {
		t.Errorf("the header has %d fields: %v", len(header), header)
	}

	issued := float64(c.at.Unix())
	for name, want := range map[string]any{
		"iss": "reader@stelfin-test.iam.gserviceaccount.com",
		// Written out rather than taken from the constant, so widening the
		// scope has to be done twice, once of those here and on purpose.
		"scope": "https://www.googleapis.com/auth/spreadsheets.readonly",
		"aud":   ts.url(),
		"iat":   issued,
		"exp":   issued + 3600,
	} {
		if claims[name] != want {
			t.Errorf("claim %s = %v, want %v", name, claims[name], want)
		}
	}
	// No subject in particular: with one the account would be asking to act as
	// a user through domain-wide delegation.
	if len(claims) != 5 {
		t.Errorf("the assertion has %d claims: %v", len(claims), claims)
	}
}

// TestATokenIsReusedUntilItIsNearlyExpired: an exchange per read would sign an
// assertion and call Google every time a range is fetched, and a token handed
// out in its last seconds expires while the request carrying it is in flight.
func TestATokenIsReusedUntilItIsNearlyExpired(t *testing.T) {
	ts := serveTokens(t, numbered)
	c := newClock()
	a := account(t, ts, c)

	want := func(token string, exchanges int) {
		t.Helper()
		got, err := a.Token(context.Background(), 42)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if got != token || ts.count() != exchanges {
			t.Fatalf("token = %q after %d exchanges, want %q after %d",
				got, ts.count(), token, exchanges)
		}
	}

	want("ya29.token-1", 1)
	want("ya29.token-1", 1)

	// A second short of the last minute, and it is still the same token.
	c.Advance(59*time.Minute - time.Second)
	want("ya29.token-1", 1)

	// Inside the last minute it is replaced, though Google would still take it.
	c.Advance(time.Second)
	want("ya29.token-2", 2)
	want("ya29.token-2", 2)
}

// TestALifetimeLongerThanAskedForIsNotBelieved: the assertion asked for an
// hour. A response claiming a century is treated as an hour, and one large
// enough to overflow a Duration does not turn into a token that expired before
// it was issued or one that never expires.
func TestALifetimeLongerThanAskedForIsNotBelieved(t *testing.T) {
	for _, claimed := range []string{"3153600000", "9223372036854775807"} {
		ts := serveTokens(t, func(n int, w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w,
				`{"access_token":"ya29.token-%d","expires_in":%s,"token_type":"Bearer"}`,
				n, claimed)
		})
		c := newClock()
		a := account(t, ts, c)

		for _, step := range []struct {
			advance   time.Duration
			exchanges int
		}{
			{0, 1},
			{58 * time.Minute, 1},
			{time.Minute, 2},
		} {
			c.Advance(step.advance)
			if _, err := a.Token(context.Background(), 42); err != nil {
				t.Fatalf("expires_in %s: %v", claimed, err)
			}
			if ts.count() != step.exchanges {
				t.Fatalf("expires_in %s: %d exchanges, want %d",
					claimed, ts.count(), step.exchanges)
			}
		}
	}
}

// TestConcurrentCallersShareOneExchange: a payroll run reads many ranges at
// once, and with a cold cache each of them would otherwise sign and post its
// own assertion.
func TestConcurrentCallersShareOneExchange(t *testing.T) {
	release := make(chan struct{})
	ts := serveTokens(t, func(n int, w http.ResponseWriter, r *http.Request) {
		<-release
		numbered(n, w, r)
	})
	// Let go on the way out as well, so a failure below is a failure and not a
	// server waiting for ever on a request nobody will release.
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)
	c := newClock()
	a := account(t, ts, c)

	const callers = 20
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = a.Token(context.Background(), 42)
		}()
	}

	// Every caller reads the clock once to look at the cache, and the one that
	// goes on to do the exchange reads it once more. When that many reads have
	// happened, all twenty are past the cache and the endpoint has still not
	// answered — which is the stampede, if there is going to be one.
	c.awaitReads(t, callers+1)
	open()
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != "ya29.token-1" {
			t.Errorf("caller %d got %q", i, tokens[i])
		}
	}
	if ts.count() != 1 {
		t.Errorf("%d exchanges for %d concurrent callers", ts.count(), callers)
	}
}

// TestACancelledFetchDoesNotFailTheCallersWaitingOnIt: sharing an exchange
// must not mean sharing the fate of whoever happened to start it. One read
// being cancelled says nothing about the key, and the reads queued behind it
// should get a token.
func TestACancelledFetchDoesNotFailTheCallersWaitingOnIt(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{})
	ts := serveTokens(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			close(entered)
			<-hold
			return
		}
		numbered(n, w, r)
	})
	// Registered after the server's own cleanup, so it runs before it and the
	// held request is let go before the server waits for it.
	t.Cleanup(func() { close(hold) })
	c := newClock()
	a := account(t, ts, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var first, second error
	var token string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, first = a.Token(ctx, 42)
	}()
	<-entered

	wg.Add(1)
	go func() {
		defer wg.Done()
		token, second = a.Token(context.Background(), 42)
	}()
	// Two reads by the first caller and one by the second, which is then
	// waiting on the first one's exchange.
	c.awaitReads(t, 3)
	cancel()
	wg.Wait()

	if !errors.Is(first, context.Canceled) {
		t.Errorf("the cancelled caller got %v", first)
	}
	if second != nil {
		t.Fatalf("the caller that was waiting failed with it: %v", second)
	}
	if token != "ya29.token-2" || ts.count() != 2 {
		t.Errorf("token = %q after %d exchanges", token, ts.count())
	}
}

// TestAWaitingCallerCanStopWaiting: the caller sharing somebody else's
// exchange still has its own context, and is not held past it.
func TestAWaitingCallerCanStopWaiting(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{})
	ts := serveTokens(t, func(n int, w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-hold
		numbered(n, w, r)
	})
	c := newClock()
	a := account(t, ts, c)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = a.Token(context.Background(), 42)
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Token(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the context's", err)
	}

	close(hold)
	wg.Wait()
	if ts.count() != 1 {
		t.Errorf("%d exchanges", ts.count())
	}
}

// TestTheContextIsHonoured: a read that has been given up on must not leave an
// exchange running behind it.
func TestTheContextIsHonoured(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{})
	ts := serveTokens(t, func(int, http.ResponseWriter, *http.Request) {
		close(entered)
		<-hold
	})
	t.Cleanup(func() { close(hold) })
	a := account(t, ts, newClock())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()

	_, err := a.Token(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, ErrTokenExchange) {
		t.Errorf("error = %v, want ErrTokenExchange as well", err)
	}
	mustNotQuote(t, err, ts.assertions()...)

	// And a context that is already over does not reach the network at all.
	before := ts.count()
	if _, err := a.Token(ctx, 42); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if ts.count() != before {
		t.Error("a request was made on a context that had already ended")
	}
}

// TestTheTokenEndpointMustBeGoogles: token_uri is a field in a file somebody
// pasted, and the assertion posted to it is a credential for the next hour.
// Each of these is an address that reads like Google's and is not.
func TestTheTokenEndpointMustBeGoogles(t *testing.T) {
	for _, bad := range []string{
		"",
		"http://oauth2.googleapis.com/token",
		"https://example.net/token",
		"https://oauth2.googleapis.com.example.net/token",
		"https://oauth2.googleapis.com@example.net/token",
		"https://user:pass@oauth2.googleapis.com/token",
		"https://oauth2.googleapis.com:8443/token",
		"https://xoauth2.googleapis.com/token",
		"https://www.googleapis.com/oauth2/v4/token",
		"https://accounts.google.com/o/oauth2/token",
		"https://oauth2.googleapis.com/token?next=https://example.net",
		"https://oauth2.googleapis.com/token#x",
		"//oauth2.googleapis.com/token",
		"oauth2.googleapis.com/token",
		"https:oauth2.googleapis.com/token",
		"https://oauth2.googleapis.com\\@example.net/token",
		" https://oauth2.googleapis.com/token",
		"ftp://oauth2.googleapis.com/token",
	} {
		_, err := NewServiceAccount(keyFile(t, map[string]any{"token_uri": bad}), nil)
		if !errors.Is(err, ErrServiceAccountKey) {
			t.Errorf("%q: error = %v, want ErrServiceAccountKey", bad, err)
		}
		mustNotQuote(t, err, keyFragments(t)...)
	}

	if _, err := NewServiceAccount(keyFile(t, map[string]any{"token_uri": nil}), nil); err == nil {
		t.Error("a key file with no token_uri was accepted")
	}
	// The real one, as Google writes it, is taken.
	if _, err := NewServiceAccount(keyFile(t, nil), nil); err != nil {
		t.Errorf("Google's own token_uri was refused: %v", err)
	}
}

// capture is a transport that records where a request was going and answers it
// itself, so a test can use Google's real address without a network.
type capture struct {
	mu   sync.Mutex
	urls []*url.URL
	body string
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.urls = append(c.urls, r.URL)
	c.body = string(raw)
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"access_token":"ya29.captured","expires_in":3599,"token_type":"Bearer"}`)),
		Request: r,
	}, nil
}

// TestNothingInTheKeyFileMovesTheAssertion: the override the other tests use
// is an option in this package and not a field. A file that tries every
// plausible spelling of one still has its assertion sent to Google, with
// Google as the audience.
func TestNothingInTheKeyFileMovesTheAssertion(t *testing.T) {
	elsewhere := "http://127.0.0.1:1/token"
	transport := &capture{}

	a, err := NewServiceAccount(keyFile(t, map[string]any{
		"token_endpoint":              elsewhere,
		"tokenEndpoint":               elsewhere,
		"tokenURI":                    elsewhere,
		"TOKEN_URI ":                  elsewhere,
		"endpoint":                    elsewhere,
		"auth_uri":                    elsewhere,
		"universe_domain":             "example.net",
		"auth_provider_x509_cert_url": elsewhere,
	}), &http.Client{Transport: transport}, WithClock(newClock().Now))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if token, err := a.Token(context.Background(), 42); err != nil || token != "ya29.captured" {
		t.Fatalf("token = %q, %v", token, err)
	}

	if len(transport.urls) != 1 {
		t.Fatalf("%d requests", len(transport.urls))
	}
	if got := transport.urls[0].String(); got != "https://oauth2.googleapis.com/token" {
		t.Errorf("the assertion went to %q", got)
	}

	form, err := url.ParseQuery(transport.body)
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	_, claims := readAssertion(t, form.Get("assertion"), &testKey().PublicKey)
	if claims["aud"] != "https://oauth2.googleapis.com/token" {
		t.Errorf("aud = %v", claims["aud"])
	}
}

// TestARedirectIsNotFollowed: a 307 replays the request body at the new
// address, and the body is the assertion. Whatever answers the exchange does
// not get to say where the credential goes next.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := serveTokens(t, numbered)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		ts := serveTokens(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.url(), status)
		})

		_, err := account(t, ts, newClock()).Token(context.Background(), 42)
		if !errors.Is(err, ErrTokenExchange) {
			t.Errorf("%d: error = %v, want ErrTokenExchange", status, err)
		}
		mustNotQuote(t, err, ts.assertions()...)
	}
	if elsewhere.count() != 0 {
		t.Errorf("the assertion followed a redirect %d times", elsewhere.count())
	}
}

// TestTheCallersClientIsLeftAsItWas: redirects are turned off on a copy. The
// client that was passed in may be shared with code that needs them.
func TestTheCallersClientIsLeftAsItWas(t *testing.T) {
	client := &http.Client{}
	if _, err := NewServiceAccount(keyFile(t, nil), client); err != nil {
		t.Fatalf("new: %v", err)
	}
	if client.CheckRedirect != nil {
		t.Error("the caller's client had its redirect policy changed")
	}
}

// TestAnAnswerThatIsNotATokenIsRefused covers each way the endpoint can answer
// without issuing something usable, and that none of them puts a credential in
// the error: not the assertion that was sent, not a token that came back, not
// the key.
func TestAnAnswerThatIsNotATokenIsRefused(t *testing.T) {
	const leaked = "ya29.must-not-be-quoted"

	for _, tc := range []struct {
		name   string
		status int
		body   string
		// says is something the error should contain, where there is something
		// worth saying.
		says string
	}{
		{"a server error", http.StatusInternalServerError, `upstream ` + leaked, "500"},
		{
			"a refused grant", http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"Invalid JWT ` + leaked + `"}`,
			"invalid_grant",
		},
		{
			"an error that is not a code", http.StatusBadRequest,
			`{"error":"` + leaked + `"}`, "400",
		},
		{
			"a token with a failing status", http.StatusUnauthorized,
			`{"access_token":"` + leaked + `","expires_in":3600,"token_type":"Bearer"}`, "401",
		},
		{"no content", http.StatusNoContent, ``, ""},
		{"no access token", http.StatusOK, `{"expires_in":3600,"token_type":"Bearer"}`, ""},
		{
			"an empty access token", http.StatusOK,
			`{"access_token":"","expires_in":3600,"token_type":"Bearer"}`, "",
		},
		{
			"a token that is not a bearer token", http.StatusOK,
			`{"access_token":"` + leaked + `","expires_in":3600,"token_type":"MAC"}`, "",
		},
		{
			"no token type", http.StatusOK,
			`{"access_token":"` + leaked + `","expires_in":3600}`, "",
		},
		{
			"no lifetime", http.StatusOK,
			`{"access_token":"` + leaked + `","token_type":"Bearer"}`, "",
		},
		{
			"a lifetime of nothing", http.StatusOK,
			`{"access_token":"` + leaked + `","expires_in":0,"token_type":"Bearer"}`, "",
		},
		{
			"a negative lifetime", http.StatusOK,
			`{"access_token":"` + leaked + `","expires_in":-3600,"token_type":"Bearer"}`, "",
		},
		{
			"a lifetime that is not a whole number", http.StatusOK,
			`{"access_token":"` + leaked + `","expires_in":"soon","token_type":"Bearer"}`, "",
		},
		{
			"a token that is not a string", http.StatusOK,
			`{"access_token":{"value":"` + leaked + `"},"expires_in":3600,"token_type":"Bearer"}`,
			"",
		},
		{"a page that is not JSON", http.StatusOK, `<html>` + leaked + `</html>`, ""},
		{"a truncated answer", http.StatusOK, `{"access_token":"` + leaked, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := serveTokens(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			a := account(t, ts, newClock())

			token, err := a.Token(context.Background(), 42)
			if !errors.Is(err, ErrTokenExchange) {
				t.Fatalf("error = %v, want ErrTokenExchange", err)
			}
			if token != "" {
				t.Errorf("a token came back with the error: %q", token)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the error does not say %q: %v", tc.says, err)
			}

			assertions := ts.assertions()
			if len(assertions) != 1 || assertions[0] == "" {
				t.Fatalf("the server saw %d assertions", len(assertions))
			}
			secrets := append(keyFragments(t), leaked, assertions[0])
			// The signature on its own is most of what makes the assertion
			// worth stealing.
			secrets = append(secrets, strings.Split(assertions[0], ".")...)
			mustNotQuote(t, err, secrets...)

			// A refusal is not remembered. The next caller asks again rather
			// than being told no from a cache for the next hour.
			if _, err := a.Token(context.Background(), 42); err == nil {
				t.Error("the second call succeeded against the same answer")
			}
			if ts.count() != 2 {
				t.Errorf("%d exchanges after two failed calls", ts.count())
			}
		})
	}
}

// TestAnOversizedAnswerIsRefused: the answer is read into memory, and how much
// is this process's decision and not the endpoint's. The body here is a
// perfectly good token response with padding after it, so it is the size being
// refused and not the content — a reader that merely stopped at the bound
// would have decoded a truncated body or, worse, accepted a prefix of one.
func TestAnOversizedAnswerIsRefused(t *testing.T) {
	const leaked = "ya29.must-not-be-quoted"
	body := `{"access_token":"` + leaked + `","expires_in":3600,"token_type":"Bearer","pad":"` +
		strings.Repeat("a", maxTokenResponseBytes) + `"}`

	ts := serveTokens(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	token, err := account(t, ts, newClock()).Token(context.Background(), 42)
	if !errors.Is(err, ErrTokenExchange) || token != "" {
		t.Fatalf("token = %q, error = %v", token, err)
	}
	if len(err.Error()) > 512 {
		t.Errorf("the error is %d bytes long", len(err.Error()))
	}
	mustNotQuote(t, err, append(ts.assertions(), leaked)...)
}

// TestAnAnswerAtTheBoundIsRead is the other side of that: the bound is a
// bound, not an off-by-one that refuses the largest answer it claims to allow.
func TestAnAnswerAtTheBoundIsRead(t *testing.T) {
	prefix := `{"access_token":"ya29.fits","expires_in":3600,"token_type":"Bearer","pad":"`
	body := prefix + strings.Repeat("a", maxTokenResponseBytes-len(prefix)-2) + `"}`
	if len(body) != maxTokenResponseBytes {
		t.Fatalf("the body is %d bytes", len(body))
	}

	ts := serveTokens(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	token, err := account(t, ts, newClock()).Token(context.Background(), 42)
	if err != nil || token != "ya29.fits" {
		t.Fatalf("token = %q, error = %v", token, err)
	}
}

// TestAKeyFileIsCheckedWhenItIsPasted: a bad key is refused at construction,
// by the person who can fix it, and the refusal never quotes the file back —
// it is mostly a private key, and the message is shown in a chat.
func TestAKeyFileIsCheckedWhenItIsPasted(t *testing.T) {
	good := pkcs8(t, testKey())
	body := good[strings.Index(good, "\n")+1 : strings.Index(good, "-----END")]

	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa: %v", err)
	}
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, tc := range []struct {
		name string
		file []byte
	}{
		{"nothing", nil},
		{"not JSON", []byte(good)},
		{"JSON cut short", keyFile(t, nil)[:900]},
		{"a JSON array", []byte(`["` + strings.ReplaceAll(body, "\n", "") + `"]`)},
		{"no type", keyFile(t, map[string]any{"type": nil})},
		{"an authorised user", keyFile(t, map[string]any{"type": "authorized_user"})},
		{"no email", keyFile(t, map[string]any{"client_email": nil})},
		{"an email that is not one", keyFile(t, map[string]any{"client_email": "reader"})},
		{
			"an email with a line break",
			keyFile(t, map[string]any{"client_email": "reader@example.net\r\nX-Injected: 1"}),
		},
		{"no key id", keyFile(t, map[string]any{"private_key_id": nil})},
		{"a key id with a space", keyFile(t, map[string]any{"private_key_id": "key id"})},
		{"no key", keyFile(t, map[string]any{"private_key": nil})},
		{"a key that is not a string", keyFile(t, map[string]any{"private_key": 12345})},
		{
			"a key that is not PEM",
			keyFile(t, map[string]any{"private_key": strings.ReplaceAll(body, "\n", "")}),
		},
		{
			"a key cut short",
			keyFile(t, map[string]any{"private_key": string(pem.EncodeToMemory(
				&pem.Block{Type: "PRIVATE KEY", Bytes: der[:len(der)/2]}))}),
		},
		{
			"a PKCS#8 key labelled PKCS#1",
			keyFile(t, map[string]any{"private_key": string(pem.EncodeToMemory(
				&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))}),
		},
		{
			"a block that is not a private key",
			keyFile(t, map[string]any{"private_key": string(pem.EncodeToMemory(
				&pem.Block{Type: "CERTIFICATE", Bytes: der}))}),
		},
		{"a key that is not RSA", keyFile(t, map[string]any{"private_key": pkcs8(t, ec)})},
		{"a key that is too short", keyFile(t, map[string]any{"private_key": pkcs8(t, short)})},
	} {
		a, err := NewServiceAccount(tc.file, nil)
		if !errors.Is(err, ErrServiceAccountKey) {
			t.Errorf("%s: error = %v, want ErrServiceAccountKey", tc.name, err)
		}
		if a != nil {
			t.Errorf("%s: an account came back with the error", tc.name)
		}
		mustNotQuote(t, err, keyFragments(t)...)
		if err != nil && len(err.Error()) > 300 {
			t.Errorf("%s: the error is %d bytes long: %v", tc.name, len(err.Error()), err)
		}
	}
}

// TestAPKCS1KeyIsAccepted: Google writes PKCS#8, and a key that has been
// through openssl comes out as PKCS#1. It is the same key and signs the same
// assertion.
func TestAPKCS1KeyIsAccepted(t *testing.T) {
	ts := serveTokens(t, numbered)
	file := keyFile(t, map[string]any{"private_key": string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey()),
	}))})

	a, err := NewServiceAccount(file, ts.srv.Client(),
		withTokenEndpoint(ts.url()), WithClock(newClock().Now))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := a.Token(context.Background(), 42); err != nil {
		t.Fatalf("token: %v", err)
	}
	readAssertion(t, ts.assertions()[0], &testKey().PublicKey)
}

// TestAnAccountBoundToAnOrgRefusesAnother: a key belongs to the community that
// pasted it, and a reader wired to the wrong account should fail on its first
// read rather than read with somebody else's credential.
func TestAnAccountBoundToAnOrgRefusesAnother(t *testing.T) {
	ts := serveTokens(t, numbered)
	a := account(t, ts, newClock(), ForOrg(42))

	if _, err := a.Token(context.Background(), 42); err != nil {
		t.Fatalf("its own org was refused: %v", err)
	}
	// Refused although there is a perfectly good token in the cache.
	if token, err := a.Token(context.Background(), 43); err == nil || token != "" {
		t.Errorf("another org was given %q, %v", token, err)
	}

	// And no account, bound or not, issues a token for no org at all.
	unbound := account(t, ts, newClock())
	for _, org := range []int64{0, -1} {
		if _, err := unbound.Token(context.Background(), org); err == nil {
			t.Errorf("org %d was given a token", org)
		}
	}
	if ts.count() != 1 {
		t.Errorf("%d exchanges, and only one call was entitled to one", ts.count())
	}

	if _, err := NewServiceAccount(keyFile(t, nil), nil, ForOrg(-1)); err == nil {
		t.Error("an account was bound to org -1")
	}
}

// TestPrintingAnAccountDoesNotPrintWhatItHolds: the default formatting of a
// struct prints its fields, and these fields are a private key and a live
// token. A stray %v in a log line should produce a name.
func TestPrintingAnAccountDoesNotPrintWhatItHolds(t *testing.T) {
	ts := serveTokens(t, numbered)
	a := account(t, ts, newClock())
	token, err := a.Token(context.Background(), 42)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		printed := fmt.Sprintf(format, a)
		if strings.Contains(printed, token) {
			t.Errorf("%s prints the token: %.80s", format, printed)
		}
		if strings.Contains(printed, testKey().D.String()) {
			t.Errorf("%s prints the private exponent", format)
		}
		for _, prime := range testKey().Primes {
			if strings.Contains(printed, prime.String()) {
				t.Errorf("%s prints a prime factor of the key", format)
			}
		}
	}
	if got := fmt.Sprint(a); !strings.Contains(got, "reader@stelfin-test") {
		t.Errorf("an account prints as %q, which does not say whose it is", got)
	}
}

// toServer sends every request to a test server, keeping the path, which is
// how the reader's own tests reach a fake Sheets API past the constant
// endpoint.
type toServer struct {
	srv *httptest.Server
}

func (ts toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(ts.srv.URL, "http://")
	return ts.srv.Client().Transport.RoundTrip(clone)
}

// TestAServiceAccountReadsASheetEndToEnd joins the two halves with nothing
// standing in for either: a key file, a real signed exchange, the reader built
// by New, and a range read with the token the exchange returned.
func TestAServiceAccountReadsASheetEndToEnd(t *testing.T) {
	google := serveTokens(t, numbered)

	var mu sync.Mutex
	var authorisations []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorisations = append(authorisations, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer ya29.token-1" {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"range":"Payroll!A1:B2","values":[["Ada",1000.50],["Bo",250]]}`))
	}))
	t.Cleanup(api.Close)

	var tokens Tokens = account(t, google, newClock(), ForOrg(42))
	reader, err := New(42, tokens, &http.Client{Transport: toServer{srv: api}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	for range 3 {
		got, err := reader.Read(context.Background(),
			connector.Request{Selector: "abc123!Payroll!A1:B2", Limit: 100})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Rows) != 2 || got.Rows[0][1].Unwrap() != "1000.50" {
			t.Fatalf("rows = %v", got.Rows)
		}
	}

	// Three reads, three authorised requests, one exchange.
	if len(authorisations) != 3 {
		t.Errorf("%d requests reached the Sheets API", len(authorisations))
	}
	if google.count() != 1 {
		t.Errorf("%d exchanges for three reads", google.count())
	}
	_, claims := readAssertion(t, google.assertions()[0], &testKey().PublicKey)
	if claims["scope"] != scope {
		t.Errorf("scope = %v", claims["scope"])
	}

	// A reader for a different org, handed this account by mistake, reads
	// nothing.
	other, err := New(43, tokens, &http.Client{Transport: toServer{srv: api}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := other.Read(context.Background(),
		connector.Request{Selector: "abc123!Payroll!A1:B2"}); err == nil {
		t.Error("another org's reader read the sheet with this account")
	}
	if len(authorisations) != 3 {
		t.Error("another org's reader reached the Sheets API")
	}
}
