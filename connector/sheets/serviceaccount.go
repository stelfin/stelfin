package sheets

import (
	"bytes"
	"context"
	"crypto"
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
	"net/url"
	"strings"
	"sync"
	"time"
)

// The Google service-account token exchange.
//
// A service account has no password and no browser. It has an RSA key, and it
// proves who it is by signing a short statement — "I am this account, I want
// this scope, from this token endpoint, for the next hour" — and posting that
// statement to Google, which answers with a bearer token. This is the OAuth 2.0
// JWT bearer grant of RFC 7523, and it is the whole of what this file does.
//
// # Why the signed statement is treated as a credential
//
// The assertion is not the key, but for an hour it is as good as one: whoever
// holds it can exchange it for a token. So everything here is arranged around
// where it is allowed to go, which is one host, over TLS, with redirects not
// followed, and never into an error or a log line. The key file is something an
// operator pasted in, and the token endpoint is a field in that file; a field
// in a pasted file does not get to choose where a credential is sent.

const (
	// scope is the one thing a token from this file can do: read spreadsheets.
	//
	// It must not be widened. This connector only ever reads, and the scope is
	// what makes that true at Google rather than merely true of this code. With
	// the read-and-write scope, a bug here, a stolen token or a stolen key is
	// somebody rewriting a community's payroll sheet — and the sheet is what a
	// payment is later proposed from. With this one it is somebody reading it.
	scope = "https://www.googleapis.com/auth/spreadsheets.readonly"

	// tokenHost is where an assertion may be sent, and tokenScheme is how.
	//
	// Constants rather than configuration, for the reason endpoint is one: the
	// assertion is a credential, and a credential whose destination can be
	// configured is one that can be sent elsewhere.
	tokenHost   = "oauth2.googleapis.com"
	tokenScheme = "https"

	// grantType names the JWT bearer grant.
	grantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	// assertionLifetime is how long an assertion is good for. An hour is the
	// most Google accepts, and also the longest a token is believed to last
	// whatever the response claims.
	assertionLifetime = time.Hour

	// refreshEarly is how long before its expiry a token stops being handed
	// out. A token that expires while the request carrying it is in flight is a
	// read that fails for no reason anybody can see; a minute is far longer
	// than a request takes and far shorter than a token lives.
	refreshEarly = time.Minute

	// maxTokenResponseBytes bounds the token endpoint's answer. A real one is
	// about a kilobyte.
	maxTokenResponseBytes = 1 << 20

	// minKeyBits is the smallest RSA modulus accepted. Google issues 2048; a
	// shorter key is not one Google made, and signing with it would be
	// presenting a weak credential as a sound one.
	minKeyBits = 2048
)

var (
	// ErrServiceAccountKey reports a key file this package will not use.
	//
	// The detail that follows it names the field at fault and never quotes the
	// file, because the file is mostly a private key and the error will be
	// shown to whoever pasted it — in a chat.
	ErrServiceAccountKey = errors.New("sheets: that is not a usable service-account key")

	// ErrTokenExchange reports that Google did not issue a token.
	ErrTokenExchange = errors.New("sheets: the token exchange failed")
)

// ServiceAccount exchanges a Google service-account key for bearer tokens.
//
// It implements Tokens. One value holds one key and the token most recently
// obtained with it, and is safe for concurrent use.
type ServiceAccount struct {
	client *http.Client
	now    func() time.Time

	email    string
	keyID    string
	key      *rsa.PrivateKey
	tokenURI string

	// org is the only org this account answers for, or zero for any.
	org int64

	// mu guards what follows. It is never held across the network call: a
	// caller waiting for an exchange waits on the flight, where it can still
	// give up when its own context ends, rather than on a lock, where it could
	// not.
	mu        sync.Mutex
	token     string
	refreshAt time.Time
	flight    *flight
}

// flight is one exchange in progress, and whatever it came back with.
type flight struct {
	done  chan struct{}
	token string
	err   error
	// abandoned records that the exchange failed because the caller running it
	// stopped waiting, which says nothing about whether Google would have
	// answered.
	abandoned bool
}

// Option adjusts a ServiceAccount at construction.
type Option func(*ServiceAccount)

// WithClock replaces the clock, so a test can move time rather than wait for
// it.
func WithClock(now func() time.Time) Option {
	return func(a *ServiceAccount) { a.now = now }
}

// ForOrg binds the account to one org, and Token then refuses any other.
//
// A key belongs to the community that pasted it. The reader already carries
// its org and passes it on every call; with this set, a reader wired to the
// wrong account is an error on the first read rather than one community's
// sheet read with another's credential.
func ForOrg(org int64) Option {
	return func(a *ServiceAccount) { a.org = org }
}

// withTokenEndpoint sends the assertion somewhere other than Google.
//
// Unexported, and that is the control: the tests in this package can point the
// exchange at a server of their own, and nothing outside the package — no
// configuration, no caller, and no field of the key file — can.
func withTokenEndpoint(to string) Option {
	return func(a *ServiceAccount) { a.tokenURI = to }
}

// NewServiceAccount reads a service-account JSON key file.
//
// Everything is checked here rather than at the first exchange, so a bad key
// is refused when it is pasted, by the person who can fix it, and not hours
// later when a payroll sheet is first read.
//
// None of the errors quote the file. The underlying parsers' own messages are
// deliberately dropped as well: a JSON syntax error reports the character it
// stopped at, and in this file that character is most likely part of the key.
func NewServiceAccount(
	keyFile []byte, client *http.Client, options ...Option,
) (*ServiceAccount, error) {
	var file struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(keyFile, &file); err != nil {
		return nil, fmt.Errorf("%w: it is not a JSON key file", ErrServiceAccountKey)
	}

	switch {
	case file.Type != "service_account":
		// An OAuth client secret and an authorised-user file are also JSON
		// from the same console, and neither can do this exchange.
		return nil, fmt.Errorf(`%w: its type is not "service_account"`, ErrServiceAccountKey)
	case !plain(file.ClientEmail) || !strings.Contains(file.ClientEmail, "@"):
		return nil, fmt.Errorf("%w: client_email is missing or malformed", ErrServiceAccountKey)
	case !plain(file.PrivateKeyID):
		return nil, fmt.Errorf("%w: private_key_id is missing or malformed", ErrServiceAccountKey)
	}

	if err := checkTokenURI(file.TokenURI); err != nil {
		return nil, err
	}
	key, err := parseKey(file.PrivateKey)
	if err != nil {
		return nil, err
	}

	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	// A copy, so the caller's client is left as it was, with redirects turned
	// off. A 307 or a 308 replays the request body at the new address, and the
	// body is the assertion: following one would let whatever answered choose
	// where the credential goes next. The redirect comes back as the response
	// instead, where it is refused for not being a 2xx.
	exchanger := *client
	exchanger.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	account := &ServiceAccount{
		client:   &exchanger,
		now:      time.Now,
		email:    file.ClientEmail,
		keyID:    file.PrivateKeyID,
		key:      key,
		tokenURI: file.TokenURI,
	}
	for _, option := range options {
		if option != nil {
			option(account)
		}
	}
	if account.now == nil {
		return nil, errors.New("sheets: a service account needs a clock")
	}
	if account.org < 0 {
		return nil, errors.New("sheets: a service account cannot be bound to that org")
	}
	return account, nil
}

// plain reports whether a field is present and could not be mistaken for
// anything else once it is inside a header or a claim.
func plain(field string) bool {
	if field == "" || len(field) > 512 {
		return false
	}
	for _, r := range field {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// checkTokenURI refuses any token endpoint but Google's.
//
// The comparison is on the parsed URL, not the string: a prefix check passes
// https://oauth2.googleapis.com.example.net and
// https://oauth2.googleapis.com@example.net, both of which are somebody else's
// server. User information, a port, a query and a fragment are refused for the
// same reason — there is no honest key file with any of them, so each is only
// a way of making an address look like one it is not.
func checkTokenURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != tokenScheme || parsed.Host != tokenHost ||
		parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%w: token_uri must be %s://%s/token, and the signed "+
			"assertion will not be sent anywhere else",
			ErrServiceAccountKey, tokenScheme, tokenHost)
	}
	return nil
}

// parseKey reads the PEM private key out of the file.
//
// Google writes PKCS#8 ("PRIVATE KEY"). PKCS#1 ("RSA PRIVATE KEY") is accepted
// too because it is what openssl produces and what a key converted by hand
// looks like. Anything else is refused by name, and the parsers' errors are
// replaced with a fixed sentence so that no fragment of the key can reach a
// message.
func parseKey(encoded string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, fmt.Errorf("%w: private_key is not PEM", ErrServiceAccountKey)
	}

	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: private_key cannot be parsed", ErrServiceAccountKey)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			// A well-formed key of another kind. Google signs these
			// assertions with RS256 and nothing else.
			return nil, fmt.Errorf("%w: private_key is not an RSA key", ErrServiceAccountKey)
		}
		key = rsaKey
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: private_key cannot be parsed", ErrServiceAccountKey)
		}
		key = parsed
	default:
		// Not echoed: the type is text from the file like any other.
		return nil, fmt.Errorf("%w: private_key is not a PEM private key", ErrServiceAccountKey)
	}

	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("%w: private_key is not a consistent RSA key",
			ErrServiceAccountKey)
	}
	if bits := key.N.BitLen(); bits < minKeyBits {
		return nil, fmt.Errorf("%w: private_key is %d bits, and %d is the least accepted",
			ErrServiceAccountKey, bits, minKeyBits)
	}
	return key, nil
}

// String names the account without what it holds.
//
// A ServiceAccount contains a private key and a live token, and the default
// formatting of a struct prints its fields. This is what a stray %v in a log
// line gets instead.
func (a *ServiceAccount) String() string {
	return "sheets: service account " + a.email
}

// GoString is String, for %#v.
func (a *ServiceAccount) GoString() string { return a.String() }

// Token returns a bearer token for the Sheets API.
//
// The token is kept and handed out again until a minute before it expires.
// Callers that arrive while one is being fetched share that fetch: a payroll
// run reading forty ranges at once makes one exchange, not forty, each of
// which would otherwise sign an assertion and spend the account's quota at the
// token endpoint.
func (a *ServiceAccount) Token(ctx context.Context, org int64) (string, error) {
	if org <= 0 {
		return "", errors.New("sheets: a token is fetched for an org")
	}
	if a.org != 0 && org != a.org {
		// Neither org is named beyond the caller's own, which it already has.
		return "", fmt.Errorf("sheets: this service account does not belong to org %d", org)
	}

	for {
		a.mu.Lock()
		if a.now().Before(a.refreshAt) && a.token != "" {
			token := a.token
			a.mu.Unlock()
			return token, nil
		}

		running := a.flight
		if running == nil {
			running = &flight{done: make(chan struct{})}
			a.flight = running
			a.mu.Unlock()
			return a.lead(ctx, running)
		}
		a.mu.Unlock()

		select {
		case <-running.done:
		case <-ctx.Done():
			// This caller stops waiting; the exchange carries on for the
			// others.
			return "", fmt.Errorf("sheets: waiting for a token: %w", ctx.Err())
		}

		if running.err == nil {
			return running.token, nil
		}
		if running.abandoned && ctx.Err() == nil {
			// The caller doing the fetching gave up, which is its business and
			// not a verdict on the key. Go round again rather than fail forty
			// reads because the first one was cancelled.
			continue
		}
		return "", running.err
	}
}

// lead runs the exchange on behalf of everyone waiting on the flight.
func (a *ServiceAccount) lead(ctx context.Context, running *flight) (string, error) {
	// Settled in a defer so that waiters are released even if a caller's
	// transport panics. Without it they would wait on a flight nobody is
	// flying until their own contexts ended.
	settled := false
	defer func() {
		if settled {
			return
		}
		a.settle(running, "", time.Time{},
			fmt.Errorf("%w: the exchange did not complete", ErrTokenExchange), false)
	}()

	token, expires, err := a.exchange(ctx)
	a.settle(running, token, expires, err, err != nil && ctx.Err() != nil)
	settled = true
	return token, err
}

// settle records an exchange's outcome and wakes whoever was waiting on it.
func (a *ServiceAccount) settle(
	running *flight, token string, expires time.Time, err error, abandoned bool,
) {
	a.mu.Lock()
	if err == nil {
		a.token = token
		a.refreshAt = expires.Add(-refreshEarly)
	}
	a.flight = nil
	a.mu.Unlock()

	running.token, running.err, running.abandoned = token, err, abandoned
	close(running.done)
}

// exchange signs an assertion and trades it for a token.
func (a *ServiceAccount) exchange(ctx context.Context) (string, time.Time, error) {
	// Read once. The expiry is measured from before the request rather than
	// after the response, so a slow exchange makes the token look shorter-lived
	// than it is and never longer.
	issued := a.now()

	assertion, err := a.assertion(issued)
	if err != nil {
		return "", time.Time{}, err
	}

	form := url.Values{"grant_type": {grantType}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, a.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		// Not wrapped, on the same principle as everywhere else here: this is
		// the call that is handed the assertion, and its error is not needed
		// to say what went wrong.
		return "", time.Time{}, fmt.Errorf("%w: the request could not be built",
			ErrTokenExchange)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		// Wrapped, because the caller needs to tell a cancelled context from a
		// refused key, and safe to: a transport error names the method and the
		// address, never the body.
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrTokenExchange, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte more than the bound, so that an answer of exactly the bound is
	// read and a longer one is known to be longer rather than quietly cut and
	// then parsed as though it were whole.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: reading the response: %w",
			ErrTokenExchange, err)
	}
	if len(raw) > maxTokenResponseBytes {
		return "", time.Time{}, fmt.Errorf("%w: the response is larger than %d bytes",
			ErrTokenExchange, maxTokenResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", time.Time{}, fmt.Errorf("%w: the token endpoint returned %s%s",
			ErrTokenExchange, resp.Status, refusal(raw))
	}

	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// The decoder's message is dropped. It can quote the value it choked
		// on, and the value may be the token.
		return "", time.Time{}, fmt.Errorf("%w: the response is not a token response",
			ErrTokenExchange)
	}

	switch {
	case body.AccessToken == "":
		return "", time.Time{}, fmt.Errorf("%w: the response carries no access_token",
			ErrTokenExchange)
	case !strings.EqualFold(body.TokenType, "Bearer"):
		// Anything else is a token this package does not know how to present.
		// Sending it as a bearer token anyway would be guessing with a
		// credential.
		return "", time.Time{}, fmt.Errorf("%w: the token is not a bearer token",
			ErrTokenExchange)
	case body.ExpiresIn <= 0:
		// A token with no lifetime cannot be cached honestly: it is either
		// already dead or good for an unknown time, and both are refused
		// rather than assumed.
		return "", time.Time{}, fmt.Errorf("%w: the token has no lifetime", ErrTokenExchange)
	}

	// Capped at what was asked for. The assertion requested an hour, so an
	// answer claiming longer is not believed — and the cap is also what keeps
	// an absurd number from overflowing a Duration into the past.
	lifetime := assertionLifetime
	if body.ExpiresIn < int64(assertionLifetime/time.Second) {
		lifetime = time.Duration(body.ExpiresIn) * time.Second
	}
	return body.AccessToken, issued.Add(lifetime), nil
}

// refusal renders Google's reason for a refusal, if it is safe to.
//
// The body is not quoted back, for the reason the reader does not quote one:
// it is Google's and it can be long. The exception is the OAuth error code —
// invalid_grant, invalid_scope — which is a fixed vocabulary, and is the
// difference between an operator knowing the key was deleted and knowing only
// that something returned 400. It is passed on only when it looks like such a
// code, so a body that echoed the assertion back in that field would not get
// through.
func refusal(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	if body.Error == "" || len(body.Error) > 40 {
		return ""
	}
	for _, r := range body.Error {
		if (r < 'a' || r > 'z') && r != '_' {
			return ""
		}
	}
	return " (" + body.Error + ")"
}

// assertion builds and signs the JWT that stands in for a password.
//
// The audience is the token endpoint, which is what stops an assertion made
// for Google being replayed at any other server that speaks this grant. The
// subject is deliberately absent: with one, the account would be asking to act
// as a named user through domain-wide delegation, which is a far larger
// authority than reading a sheet that was shared with it.
func (a *ServiceAccount) assertion(issued time.Time) (string, error) {
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}{"RS256", "JWT", a.keyID})
	if err != nil {
		return "", fmt.Errorf("%w: the assertion could not be built", ErrTokenExchange)
	}
	claims, err := json.Marshal(struct {
		Issuer   string `json:"iss"`
		Scope    string `json:"scope"`
		Audience string `json:"aud"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}{a.email, scope, a.tokenURI, issued.Unix(), issued.Add(assertionLifetime).Unix()})
	if err != nil {
		return "", fmt.Errorf("%w: the assertion could not be built", ErrTokenExchange)
	}

	var signing bytes.Buffer
	signing.WriteString(base64.RawURLEncoding.EncodeToString(header))
	signing.WriteByte('.')
	signing.WriteString(base64.RawURLEncoding.EncodeToString(claims))

	digest := sha256.Sum256(signing.Bytes())
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		// Not wrapped. Nothing the signer says is known to carry key material,
		// and nothing it says would help the person reading this either.
		return "", fmt.Errorf("%w: the assertion could not be signed", ErrTokenExchange)
	}

	signing.WriteByte('.')
	signing.WriteString(base64.RawURLEncoding.EncodeToString(signature))
	return signing.String(), nil
}
