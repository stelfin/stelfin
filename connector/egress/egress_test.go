package egress_test

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stelfin/stelfin/connector/egress"
	"github.com/stelfin/stelfin/connector/egress/egresstest"
)

// resolver is a stand-in for DNS that answers from a script: the first lookup
// gets the first answer, the second the second, and the last answer repeats.
// It records every name it was asked for, which is how a test counts lookups.
type resolver struct {
	mu      sync.Mutex
	answers [][]string
	asked   []string
}

func (r *resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.asked = append(r.asked, host)
	if len(r.answers) == 0 {
		return nil, nil
	}
	i := len(r.asked) - 1
	if i >= len(r.answers) {
		i = len(r.answers) - 1
	}
	out := make([]netip.Addr, 0, len(r.answers[i]))
	for _, text := range r.answers[i] {
		out = append(out, netip.MustParseAddr(text))
	}
	return out, nil
}

func (r *resolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

// errStop ends a dial once the test has seen what it came for.
var errStop = errors.New("the test stops here")

// recorder is a dial function that connects to nothing and remembers every
// address it was asked to connect to.
type recorder struct {
	mu     sync.Mutex
	dialed []string
}

func (r *recorder) dial(_ context.Context, _, address string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialed = append(r.dialed, address)
	return nil, errStop
}

func (r *recorder) addresses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

// get makes one request and returns its error, closing any response.
func get(t *testing.T, client *egress.Client, target string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// TestEveryNonPublicRangeIsRefused is the list this package exists to hold.
//
// Each entry is somewhere a request from inside a deployment reaches something
// that a request from outside cannot: the instance's credentials, a database, a
// neighbour. One row per range, so that a range dropped from the list fails by
// name.
func TestEveryNonPublicRangeIsRefused(t *testing.T) {
	for name, addrs := range map[string][]string{
		"loopback":            {"127.0.0.1", "127.255.255.254", "::1"},
		"rfc1918":             {"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.255.255"},
		"link-local":          {"169.254.0.1", "fe80::1", "febf::1"},
		"cloud metadata":      {"169.254.169.254"},
		"carrier-grade NAT":   {"100.64.0.1", "100.127.255.255"},
		"unique-local":        {"fc00::1", "fd12:3456:789a::1"},
		"unspecified":         {"0.0.0.0", "0.1.2.3", "::"},
		"multicast":           {"224.0.0.1", "239.255.255.255", "ff02::1"},
		"broadcast":           {"255.255.255.255"},
		"reserved":            {"240.0.0.1", "192.0.0.1"},
		"documentation":       {"192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", "3fff::1"},
		"benchmarking":        {"198.18.0.1", "198.19.255.255", "2001:2::1"},
		"mapped loopback":     {"::ffff:127.0.0.1"},
		"mapped private":      {"::ffff:10.0.0.1", "::ffff:192.168.1.1"},
		"mapped metadata":     {"::ffff:169.254.169.254"},
		"IPv4-compatible":     {"::127.0.0.1", "::10.0.0.1"},
		"NAT64":               {"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::a00:1"},
		"6to4":                {"2002:7f00:1::1", "2002:a00:1::1", "192.88.99.1"},
		"Teredo":              {"2001:0:4136:e378:8000:63bf:3fff:fdd2"},
		"site-local":          {"fec0::1"},
		"discard":             {"100::1"},
		"segment routing":     {"5f00::1"},
		"outside 2000::/3":    {"4000::1", "8000::1", "e000::1"},
		"with a zone":         {"fe80::1%eth0", "2606:4700::1%eth0"},
		"IPv4-translated":     {"::ffff:0:127.0.0.1"},
		"the zero netip.Addr": {""},
	} {
		t.Run(name, func(t *testing.T) {
			for _, text := range addrs {
				var addr netip.Addr
				if text != "" {
					addr = netip.MustParseAddr(text)
				}
				if err := egress.CheckAddr(addr); !errors.Is(err, egress.ErrForbiddenAddress) {
					t.Errorf("CheckAddr(%q) = %v, want ErrForbiddenAddress", text, err)
				}
			}
		})
	}
}

// TestPublicAddressesArePermitted: a list that refuses everything would pass
// the test above, and would also be a connector layer that connects to nothing.
// The addresses either side of each refused range are the interesting ones.
func TestPublicAddressesArePermitted(t *testing.T) {
	for _, text := range []string{
		"1.1.1.1", "8.8.8.8", "9.255.255.255", "11.0.0.0",
		"100.63.255.255", "100.128.0.0", "126.255.255.255", "128.0.0.1",
		"169.253.255.255", "169.255.0.0", "172.15.255.255", "172.32.0.0",
		"192.167.255.255", "192.169.0.0", "198.17.255.255", "198.20.0.0",
		"223.255.255.255",
		"2606:4700:4700::1111", "2a00:1450:4001::1", "2001:4860:4860::8888",
		"::ffff:8.8.8.8",
	} {
		if err := egress.CheckAddr(netip.MustParseAddr(text)); err != nil {
			t.Errorf("CheckAddr(%s) = %v, want it permitted", text, err)
		}
	}
}

// TestGoDoesNotReadADisguisedAddressAsOne records what the parser underneath
// actually does, because the URL rules lean on it.
//
// The C library reads all of these as 127.0.0.1. net/netip reads none of them
// as an address at all — so in this process they are host names, and the host
// name rules have to be the ones that refuse them. If a Go release ever
// loosened the parser, this is the test that would say so.
func TestGoDoesNotReadADisguisedAddressAsOne(t *testing.T) {
	for _, disguise := range []string{
		"0177.0.0.1",   // octal
		"0x7f.0.0.1",   // hex
		"2130706433",   // one 32-bit integer
		"127.1",        // two-part shorthand
		"127.0.1",      // three-part shorthand
		"0x7f000001",   // one hex integer
		"127.0.0.01",   // a leading zero
		"017700000001", // one octal integer
	} {
		if addr, err := netip.ParseAddr(disguise); err == nil {
			t.Errorf("netip.ParseAddr(%q) = %s; the URL rules assume it is refused", disguise, addr)
		}
		if err := egress.ValidateURL("https://" + disguise + "/"); !errors.Is(err, egress.ErrBadURL) {
			t.Errorf("ValidateURL(https://%s/) = %v, want ErrBadURL", disguise, err)
		}
	}
}

// TestAURLOfTheWrongShapeIsRefusedAtRegistration covers the syntactic rules,
// each of which closes one way of making a URL go somewhere other than where
// it appears to.
func TestAURLOfTheWrongShapeIsRefusedAtRegistration(t *testing.T) {
	for name, raw := range map[string]string{
		"plain http":           "http://example.com/hook",
		"another scheme":       "ftp://example.com/hook",
		"no scheme":            "example.com/hook",
		"userinfo":             "https://user:pass@example.com/hook",
		"userinfo as a decoy":  "https://sheets.googleapis.com@203.0.113.9/",
		"a username alone":     "https://admin@example.com/",
		"another port":         "https://example.com:8443/hook",
		"the redis port":       "https://example.com:6379/",
		"port zero":            "https://example.com:0/",
		"no host":              "https:///hook",
		"an opaque URL":        "https:example.com",
		"a loopback literal":   "https://127.0.0.1/",
		"a private literal":    "https://10.1.2.3/",
		"the metadata address": "https://169.254.169.254/latest/meta-data/",
		"an IPv6 loopback":     "https://[::1]/",
		"a mapped loopback":    "https://[::ffff:127.0.0.1]/",
		"a zoned literal":      "https://[fe80::1%25eth0]/",
		"a single label":       "https://localhost/",
		"a bare internal name": "https://metadata/computeMetadata/v1/",
		"a trailing dot":       "https://example.com./",
		"a doubled dot":        "https://example..com/",
		"a leading hyphen":     "https://-example.com/",
		"an underscore":        "https://ex_ample.com/",
		"a non-ASCII name":     "https://exämple.com/",
		"a numeric last label": "https://example.123/",
		"a hex last label":     "https://example.0x7f/",
		"a backslash":          "https://example.com\\@evil.test/",
		"whitespace in a host": "https://exam ple.com/",
		"a control character":  "https://example.com/\x00",
		"an overlong URL":      "https://example.com/" + strings.Repeat("a", 4096),
		"the empty string":     "",
	} {
		t.Run(name, func(t *testing.T) {
			if err := egress.ValidateURL(raw); !errors.Is(err, egress.ErrBadURL) {
				t.Errorf("ValidateURL(%q) = %v, want ErrBadURL", raw, err)
			}
		})
	}

	for _, raw := range []string{
		"https://example.com/hook",
		"https://example.com:443/hook?x=1",
		"https://API.Example.com/",
		"https://xn--exmple-cua.com/",
		"https://8.8.8.8/",
		"https://[2606:4700:4700::1111]/",
	} {
		if err := egress.ValidateURL(raw); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want it accepted", raw, err)
		}
	}
}

// TestARegistrationErrorDoesNotQuoteTheURL: a webhook address can carry a
// secret in its query, and the error from registering it is shown in a chat.
func TestARegistrationErrorDoesNotQuoteTheURL(t *testing.T) {
	err := egress.ValidateURL("http://example.com/hook?key=hunter2-secret")
	if err == nil {
		t.Fatal("an http URL was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error quotes the query: %v", err)
	}

	err = egress.ValidateURL("https://example.com/%zz?key=hunter2-secret")
	if err == nil {
		t.Fatal("an unparseable URL was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error quotes the query: %v", err)
	}
}

// TestTheCheckedAddressIsDialledNotTheName is the pin.
//
// If the name reached the dialler, the dialler would resolve it again, and the
// check would have been made on an answer that was never used.
func TestTheCheckedAddressIsDialledNotTheName(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	if err := get(t, client, "https://hooks.example.com/payroll"); !errors.Is(err, errStop) {
		t.Fatalf("get = %v, want the dial to have been reached", err)
	}

	got := rec.addresses()
	if len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %v, want exactly 93.184.216.34:443", got)
	}
	if dns.lookups() != 1 {
		t.Errorf("%d lookups, want 1", dns.lookups())
	}
}

// TestARebindingResolverCannotMoveTheConnection is the attack the pin is for.
//
// The name answers with a public address the first time it is asked and with
// the metadata service the second. A client that checks and then dials by name
// asks twice, and connects to the second answer having approved the first.
func TestARebindingResolverCannotMoveTheConnection(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}, {"169.254.169.254"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	if err := get(t, client, "https://rebind.example.com/"); !errors.Is(err, errStop) {
		t.Fatalf("get = %v, want the dial to have been reached", err)
	}

	if dns.lookups() != 1 {
		t.Fatalf("the name was resolved %d times; a second lookup is a second answer", dns.lookups())
	}
	got := rec.addresses()
	if len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %v, want only the address that was checked", got)
	}
}

// TestOnePrivateAnswerRefusesTheWholeName: a name that answers with a public
// address and a private one is not half trustworthy. Which one a connection
// lands on would be down to ordering, and the owner chose to list both.
func TestOnePrivateAnswerRefusesTheWholeName(t *testing.T) {
	for _, answers := range [][]string{
		{"93.184.216.34", "10.0.0.5"},
		{"10.0.0.5", "93.184.216.34"},
		{"93.184.216.34", "::ffff:127.0.0.1"},
		{"2606:4700::1", "fd00::1"},
	} {
		dns := &resolver{answers: [][]string{answers}}
		rec := &recorder{}
		client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

		err := get(t, client, "https://mixed.example.com/")
		if !errors.Is(err, egress.ErrForbiddenAddress) {
			t.Errorf("%v: get = %v, want ErrForbiddenAddress", answers, err)
		}
		if got := rec.addresses(); len(got) != 0 {
			t.Errorf("%v: dialled %v, want nothing dialled", answers, got)
		}
	}
}

// TestANameThatResolvesToAPrivateAddressIsNeverDialled is the ordinary case:
// no rebinding, just a public name somebody pointed at an internal address.
// The URL passes every syntactic check, which is why those are not the control.
func TestANameThatResolvesToAPrivateAddressIsNeverDialled(t *testing.T) {
	for _, private := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1", "::ffff:10.0.0.1",
		"64:ff9b::a00:1", "2002:a00:1::1",
	} {
		if err := egress.ValidateURL("https://innocent.example.com/"); err != nil {
			t.Fatalf("the URL should pass registration: %v", err)
		}

		dns := &resolver{answers: [][]string{{private}}}
		rec := &recorder{}
		client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

		err := get(t, client, "https://innocent.example.com/")
		if !errors.Is(err, egress.ErrForbiddenAddress) {
			t.Errorf("%s: get = %v, want ErrForbiddenAddress", private, err)
		}
		if got := rec.addresses(); len(got) != 0 {
			t.Errorf("%s: dialled %v", private, got)
		}
	}
}

// TestALiteralAddressIsCheckedWithoutBeingResolved: an address needs no lookup,
// and sending one to a resolver would be asking a third party to reinterpret
// it.
func TestALiteralAddressIsCheckedWithoutBeingResolved(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	if err := get(t, client, "https://8.8.8.8/"); !errors.Is(err, errStop) {
		t.Fatalf("a public literal: %v", err)
	}
	if got := rec.addresses(); len(got) != 1 || got[0] != "8.8.8.8:443" {
		t.Errorf("dialled %v", got)
	}

	for _, target := range []string{
		"https://127.0.0.1/", "https://[::1]/", "https://169.254.169.254/",
		"https://[::ffff:10.0.0.1]/",
	} {
		if err := get(t, client, target); !errors.Is(err, egress.ErrForbiddenAddress) {
			t.Errorf("%s: %v, want ErrForbiddenAddress", target, err)
		}
	}

	if dns.lookups() != 0 {
		t.Errorf("%d lookups for literal addresses, want none", dns.lookups())
	}
	if got := rec.addresses(); len(got) != 1 {
		t.Errorf("dialled %v, want only the public literal", got)
	}
}

// TestAHostWithNoAddressIsAnError: an empty answer must not fall through to a
// dial of nothing, or to a default.
func TestAHostWithNoAddressIsAnError(t *testing.T) {
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: &resolver{}, Dial: rec.dial})

	if err := get(t, client, "https://nowhere.example.com/"); !errors.Is(err, egress.ErrNoAddress) {
		t.Fatalf("get = %v, want ErrNoAddress", err)
	}
	if got := rec.addresses(); len(got) != 0 {
		t.Errorf("dialled %v", got)
	}
}

// TestTheRulesHoldForARequestThatWasNeverRegistered: ValidateURL is advice at
// registration, and a caller can build a request without asking it. The same
// rules are applied on the way out, before anything is resolved.
func TestTheRulesHoldForARequestThatWasNeverRegistered(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	for _, target := range []string{
		"http://example.com/",
		"https://user:pass@example.com/",
		"https://example.com:8443/",
		"https://localhost/",
	} {
		if err := get(t, client, target); !errors.Is(err, egress.ErrBadURL) {
			t.Errorf("%s: %v, want ErrBadURL", target, err)
		}
	}
	if dns.lookups() != 0 || len(rec.addresses()) != 0 {
		t.Errorf("a refused URL was resolved %d times and dialled %v",
			dns.lookups(), rec.addresses())
	}
}

// TestAnotherPortIsDialledOnlyWhenAllowed: the restriction is a default, not a
// law, but lifting it is something a deployment does by name.
func TestAnotherPortIsDialledOnlyWhenAllowed(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{
		Resolver: dns, Dial: rec.dial, ExtraPorts: []uint16{8443},
	})

	if err := client.CheckURL("https://example.com:8443/"); err != nil {
		t.Errorf("CheckURL on an allowed port: %v", err)
	}
	if err := egress.ValidateURL("https://example.com:8443/"); !errors.Is(err, egress.ErrBadURL) {
		t.Errorf("the strict check accepted a port only one client allows: %v", err)
	}
	if err := get(t, client, "https://example.com:8443/"); !errors.Is(err, errStop) {
		t.Fatalf("get on an allowed port: %v", err)
	}
	if err := get(t, client, "https://example.com:9000/"); !errors.Is(err, egress.ErrBadURL) {
		t.Errorf("get on another port: %v, want ErrBadURL", err)
	}
	if got := rec.addresses(); len(got) != 1 || got[0] != "93.184.216.34:8443" {
		t.Errorf("dialled %v", got)
	}
}

// TestTheEnvironmentProxyIsIgnored: through a proxy the connection goes to the
// proxy and the destination is resolved on its far side, by something that
// applies none of this. The pin would be on the wrong hop.
func TestTheEnvironmentProxyIsIgnored(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.internal.example:3128")
	t.Setenv("https_proxy", "http://proxy.internal.example:3128")
	t.Setenv("ALL_PROXY", "http://proxy.internal.example:3128")

	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	if err := get(t, client, "https://hooks.example.com/"); !errors.Is(err, errStop) {
		t.Fatalf("get = %v", err)
	}
	if len(dns.asked) != 1 || dns.asked[0] != "hooks.example.com" {
		t.Errorf("resolved %v, want only the destination", dns.asked)
	}
	if got := rec.addresses(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Errorf("dialled %v, want the destination and not a proxy", got)
	}
}

// TestAnErrorDoesNotCarryTheURL: net/http quotes the full request URL in every
// failure it reports, and a webhook address may hold a secret in its query.
func TestAnErrorDoesNotCarryTheURL(t *testing.T) {
	dns := &resolver{answers: [][]string{{"93.184.216.34"}}}
	rec := &recorder{}
	client := egress.NewClient(egress.Options{Resolver: dns, Dial: rec.dial})

	err := get(t, client, "https://hooks.example.com/in?key=hunter2-secret")
	if err == nil {
		t.Fatal("the dial was meant to fail")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error carries the query: %v", err)
	}
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		t.Errorf("the error still wraps a url.Error: %v", err)
	}
}

// tlsServer stands up a loopback TLS server and returns it with its port.
func tlsServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatalf("server address: %v", err)
	}
	return srv, port
}

// TestLoopbackIsRefusedWithoutTheTestHook: every other test that talks to a
// server does so through the relaxation, so this is the one that shows the
// relaxation is what made them possible.
func TestLoopbackIsRefusedWithoutTheTestHook(t *testing.T) {
	var hits int
	srv, port := tlsServer(t, func(http.ResponseWriter, *http.Request) { hits++ })

	portNumber, err := netip.ParseAddrPort("127.0.0.1:" + port)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	client := egress.NewClient(egress.Options{ExtraPorts: []uint16{portNumber.Port()}})

	if err := get(t, client, srv.URL); !errors.Is(err, egress.ErrForbiddenAddress) {
		t.Fatalf("get = %v, want ErrForbiddenAddress", err)
	}
	if hits != 0 {
		t.Errorf("the loopback server was reached %d times", hits)
	}
}

// TestTheTestHookPermitsLoopbackAndNothingElse: the relaxation is for reaching
// a test server. It must not also be a way to reach a private network, or the
// tests of the connectors would be exercising a client with no checks at all.
func TestTheTestHookPermitsLoopbackAndNothingElse(t *testing.T) {
	srv, _ := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	rec := &recorder{}
	for _, private := range []string{
		"10.0.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "fc00::1", "fe80::1", "0.0.0.0",
	} {
		dns := &resolver{answers: [][]string{{private}}}
		client := egresstest.Client(t, srv, egress.Options{Resolver: dns, Dial: rec.dial})
		if err := get(t, client, "https://internal.example.com/"); !errors.Is(err, egress.ErrForbiddenAddress) {
			t.Errorf("%s through the test hook: %v, want ErrForbiddenAddress", private, err)
		}
	}
	if got := rec.addresses(); len(got) != 0 {
		t.Errorf("dialled %v", got)
	}

	relaxed := egresstest.Client(t, srv, egress.Options{})
	if err := get(t, relaxed, strings.Replace(srv.URL, "https://", "http://", 1)); !errors.Is(err, egress.ErrBadURL) {
		t.Errorf("plain http through the test hook: %v, want ErrBadURL", err)
	}
	if err := get(t, relaxed, srv.URL); err != nil {
		t.Errorf("the test server itself: %v", err)
	}
}

// TestTheCertificateIsStillVerifiedAgainstTheName: pinning the address must
// narrow where a connection goes without widening who may answer it. The socket
// is opened to a number, and the certificate still has to be for the name.
//
// httptest's certificate is issued for example.com, so a name that resolves to
// the same server and is not example.com is exactly a server presenting
// somebody else's certificate.
func TestTheCertificateIsStillVerifiedAgainstTheName(t *testing.T) {
	var hits int
	srv, port := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	})
	dns := &resolver{answers: [][]string{{"127.0.0.1"}}}
	client := egresstest.Client(t, srv, egress.Options{Resolver: dns})

	if err := get(t, client, "https://example.com:"+port+"/"); err != nil {
		t.Fatalf("the name on the certificate: %v", err)
	}
	if hits != 1 {
		t.Fatalf("%d requests reached the server, want 1", hits)
	}

	err := get(t, client, "https://impostor.example.org:"+port+"/")
	var hostname x509.HostnameError
	if !errors.As(err, &hostname) {
		t.Fatalf("a name the certificate does not cover: %v, want a hostname error", err)
	}
	if hits != 1 {
		t.Errorf("a request was served under the wrong name")
	}
}

// TestARedirectIsNotFollowed: a redirect is a second URL, chosen by the server.
// Here it points at a handler that counts, standing in for the internal address
// a hostile endpoint would send the request to.
func TestARedirectIsNotFollowed(t *testing.T) {
	var followed int
	status := http.StatusFound
	srv, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal" {
			followed++
			return
		}
		http.Redirect(w, r, "/internal", status)
	})
	client := egresstest.Client(t, srv, egress.Options{})

	for _, status = range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		if err := get(t, client, srv.URL+"/start"); !errors.Is(err, egress.ErrRedirect) {
			t.Fatalf("%d: get = %v, want ErrRedirect", status, err)
		}
	}
	if followed != 0 {
		t.Errorf("the redirect was followed %d times", followed)
	}
}

// TestASilentServerRunsOutOfBudget: an endpoint that accepts a connection and
// then says nothing must cost a bounded wait, not a request path held open.
func TestASilentServerRunsOutOfBudget(t *testing.T) {
	release := make(chan struct{})
	srv, _ := tlsServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	for name, options := range map[string]egress.Options{
		"waiting for headers": {ResponseHeaderTimeout: 50 * time.Millisecond},
		"the whole exchange":  {Timeout: 50 * time.Millisecond},
	} {
		client := egresstest.Client(t, srv, options)
		started := time.Now()
		err := get(t, client, srv.URL)
		if err == nil {
			t.Fatalf("%s: a silent server produced a response", name)
		}
		if waited := time.Since(started); waited > 5*time.Second {
			t.Errorf("%s: waited %s", name, waited)
		}
	}
}

// TestASlowBodyIsCutOffByTheOverallBudget: headers arriving promptly are not a
// promise about the body, and the header deadline has passed by the time a
// server starts to dribble.
func TestASlowBodyIsCutOffByTheOverallBudget(t *testing.T) {
	release := make(chan struct{})
	srv, _ := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	})
	t.Cleanup(func() { close(release) })

	client := egresstest.Client(t, srv, egress.Options{Timeout: 100 * time.Millisecond})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the headers were sent promptly: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := egress.ReadBody(resp.Body, 1<<20); err == nil {
		t.Fatal("a body that never ended was read to the end")
	}
}

// TestAnOversizedBodyIsAnErrorNotATruncation: half a document parses. The
// failure with no symptom is the one where the first megabyte of a payroll
// export is read, believed, and paid.
func TestAnOversizedBodyIsAnErrorNotATruncation(t *testing.T) {
	if got, err := egress.ReadBody(strings.NewReader("12345678"), 8); err != nil || string(got) != "12345678" {
		t.Errorf("a body exactly at the cap: %q, %v", got, err)
	}
	if got, err := egress.ReadBody(strings.NewReader(""), 8); err != nil || len(got) != 0 {
		t.Errorf("an empty body: %q, %v", got, err)
	}

	got, err := egress.ReadBody(strings.NewReader("123456789"), 8)
	if !errors.Is(err, egress.ErrBodyTooLarge) {
		t.Fatalf("a body one byte over: %q, %v, want ErrBodyTooLarge", got, err)
	}

	// The streaming form fails the read that crosses the line, so a decoder
	// on top of it stops with an error rather than a short document.
	limited := egress.LimitBody(strings.NewReader(strings.Repeat("x", 100)), 10)
	n, err := io.Copy(io.Discard, limited)
	if !errors.Is(err, egress.ErrBodyTooLarge) {
		t.Fatalf("copy = %d, %v, want ErrBodyTooLarge", n, err)
	}
	if n > 10 {
		t.Errorf("%d bytes were delivered past a cap of 10", n)
	}
}
