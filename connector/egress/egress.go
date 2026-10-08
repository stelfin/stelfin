// Package egress is the only way stelfin dials a URL an org supplied.
//
// A connector's address is typed by somebody outside this deployment, and the
// request to it is made from inside. That asymmetry is the whole of server-side
// request forgery: the org chooses the destination, and stelfin lends it a
// network position — next to the metadata service, the database, and every
// internal port that trusts a caller for being local.
//
// # Resolve once, check, and dial the address
//
// Checking a hostname is not checking a destination. Resolving a name and
// connecting to it are two operations, and whoever runs the name's DNS answers
// both: a public address for the lookup that is checked, a private one a
// moment later for the lookup that is used. So here there is one lookup. Every
// address it returns is checked, and the connection is made to one of those
// addresses as a literal — the name is never handed to anything that would
// resolve it again.
//
// TLS is unaffected. The transport still verifies the certificate against the
// name in the URL, so pinning the address narrows where a connection may go
// without widening who may answer it.
//
// # No redirects, no proxy
//
// A redirect is a second URL chosen by the server, and it would need the same
// treatment as the first: validated, resolved once, checked, pinned. The simple
// correct answer is not to follow them. A connector's address is registered,
// and an endpoint that has moved is re-registered by a person rather than
// followed by a process.
//
// The proxy settings in the environment are ignored. Through a proxy the
// connection is made to the proxy and the real destination is resolved on its
// far side, by something that applies none of these checks — the pin would be
// on the wrong hop and mean nothing.
//
// # Budgets
//
// Every phase of a request has its own deadline — connecting, the handshake,
// waiting for headers, the whole exchange — and a body is read through a hard
// cap that reports an overrun as an error. An endpoint that is slow, hostile or
// merely broken costs a bounded amount of time and memory and then fails.
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stelfin/stelfin/connector/egress/internal/relax"
)

var (
	// ErrForbiddenAddress reports an address stelfin will not connect to:
	// loopback, private, link-local, or any other range where a request could
	// reach something that trusts it for where it came from.
	ErrForbiddenAddress = errors.New("egress: that address is not a public one")

	// ErrBadURL reports a URL of a shape stelfin will not fetch.
	ErrBadURL = errors.New("egress: that URL cannot be used")

	// ErrNoAddress reports a host name that resolved to nothing.
	ErrNoAddress = errors.New("egress: that host has no address")

	// ErrRedirect reports an endpoint that answered with a redirect.
	//
	// An error rather than a response handed back for the caller to inspect,
	// because the only two things a caller could do with a 3xx are follow it,
	// which is the thing being prevented, or fail.
	ErrRedirect = errors.New("egress: the endpoint redirected, and redirects are not followed")

	// ErrBodyTooLarge reports a response larger than its cap.
	//
	// An error rather than a truncation. Half a payroll export is a document
	// that parses, and the people in the missing half are not paid with
	// nothing saying so.
	ErrBodyTooLarge = errors.New("egress: the response is larger than this deployment will read")
)

// Resolver turns a host name into addresses.
//
// An interface so that the one resolution a dial performs can be observed and
// faked in a test. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc opens a connection to an address.
//
// The address it is given is always an IP literal and a port that have already
// been checked, never a name.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Options configures a Client. The zero value is the strictest client and the
// one a deployment should use.
type Options struct {
	// Resolver looks a name up. Nil means the Go resolver.
	Resolver Resolver
	// Dial opens the connection. Nil means a net.Dialer that re-checks the
	// address at the socket.
	Dial DialFunc

	// ExtraPorts are ports permitted in addition to 443.
	//
	// Empty by default, and worth leaving so: a public address is not a
	// promise that everything listening on it is a web server.
	ExtraPorts []uint16

	// Timeout bounds one whole exchange, from dialling to the last byte of
	// the body. Zero means DefaultTimeout.
	Timeout time.Duration
	// DialTimeout bounds resolving and connecting. Zero means
	// DefaultDialTimeout.
	DialTimeout time.Duration
	// TLSHandshakeTimeout bounds the handshake. Zero means
	// DefaultTLSHandshakeTimeout.
	TLSHandshakeTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait between the request being written
	// and the response headers arriving. Zero means
	// DefaultResponseHeaderTimeout.
	ResponseHeaderTimeout time.Duration

	// testing is the relaxation ForTesting applies. Unexported, and settable
	// only with a value of a type nothing outside this directory can name.
	testing *relax.Token
}

// The budgets a client has when none are given.
const (
	DefaultTimeout               = 20 * time.Second
	DefaultDialTimeout           = 5 * time.Second
	DefaultTLSHandshakeTimeout   = 5 * time.Second
	DefaultResponseHeaderTimeout = 10 * time.Second
)

// ForTesting returns options that also permit a loopback test server.
//
// Not callable from outside connector/egress: the token's type lives in an
// internal package, so no other code can produce the argument. The way in for a
// test is the egresstest package, which is the only caller.
func ForTesting(options Options, token relax.Token) Options {
	options.testing = &token
	return options
}

// Client makes requests to addresses an org supplied.
//
// A type of its own rather than a bare *http.Client, so that a connector which
// accepts one cannot be handed a client that skips the checks: the signature
// says where the request goes through.
type Client struct {
	http   *http.Client
	policy policy
}

// NewClient returns a Client.
func NewClient(options Options) *Client {
	p := policy{ports: map[uint16]bool{defaultPort: true}}
	for _, port := range options.ExtraPorts {
		if port != 0 {
			p.ports[port] = true
		}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if options.testing != nil {
		p.relaxed = true
		tlsConfig.RootCAs = options.testing.Roots
	}

	d := &dialer{
		policy:   p,
		resolver: options.Resolver,
		dial:     options.Dial,
		timeout:  orDefault(options.DialTimeout, DefaultDialTimeout),
	}
	if d.resolver == nil {
		// The Go resolver rather than the C library's. getaddrinfo reads
		// "2130706433" and "0x7f.1" as addresses; the Go resolver treats them
		// as names. Either answer would be checked, but the fewer parsers
		// with opinions about what an address looks like, the better.
		d.resolver = &net.Resolver{PreferGo: true}
	}
	if d.dial == nil {
		d.dial = (&net.Dialer{Control: d.control}).DialContext
	}

	transport := &http.Transport{
		// Nil, not ProxyFromEnvironment. A proxy would make the connection
		// somebody else's to resolve, and the pinned address would be the
		// proxy's.
		Proxy:       nil,
		DialContext: d.dialContext,
		// ServerName is left empty, so the transport fills it from the URL's
		// host for each connection. The certificate is verified against the
		// name the org registered, although the socket was opened to a
		// number.
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    orDefault(options.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout),
		ResponseHeaderTimeout:  orDefault(options.ResponseHeaderTimeout, DefaultResponseHeaderTimeout),
		MaxResponseHeaderBytes: 64 << 10,
		// A kept-alive connection is one to an address that was checked when
		// it was opened, so reusing it is safe. It is kept briefly all the
		// same: the longer a connection lives, the older the answer it was
		// pinned to.
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}

	return &Client{
		policy: p,
		http: &http.Client{
			Transport: guarded{policy: p, next: transport},
			Timeout:   orDefault(options.Timeout, DefaultTimeout),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrRedirect
			},
		},
	}
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// CheckURL is ValidateURL under this client's policy, so a deployment that has
// permitted an extra port validates registrations against the same rule its
// requests are held to.
func (c *Client) CheckURL(raw string) error {
	return checkRawURL(raw, c.policy)
}

// Do sends a request.
//
// The error never contains the URL. net/http wraps every failure in one that
// quotes the full request URL, and a registered webhook address may carry a
// secret in its query — so the wrapper is taken off here, once, rather than
// trusted to be taken off by each caller before the error reaches a chat.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			err = wrapped.Err
		}
		if strings.HasPrefix(err.Error(), "egress: ") {
			// Already this package's own refusal, and already says why.
			return nil, err
		}
		return nil, fmt.Errorf("egress: request failed: %w", err)
	}
	return resp, nil
}

// guarded applies the URL rules to every request, not only to registrations.
//
// ValidateURL is advice given at registration. This is the same check made
// where it cannot be skipped: a caller that builds a request for http://, or
// for a port nobody allowed, is refused before anything is resolved.
type guarded struct {
	policy policy
	next   http.RoundTripper
}

func (g guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkURL(req.URL, g.policy); err != nil {
		return nil, err
	}
	return g.next.RoundTrip(req)
}

// dialer is the check that counts.
type dialer struct {
	policy   policy
	resolver Resolver
	dial     DialFunc
	timeout  time.Duration
}

// maxDialAttempts bounds how many of a name's addresses are tried. A name with
// forty unreachable addresses should not get forty connection timeouts.
const maxDialAttempts = 4

// dialContext resolves a host once, checks every answer, and connects to one of
// the checked addresses.
func (d *dialer) dialContext(ctx context.Context, _, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: it has no host and port", ErrBadURL)
	}
	n, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || !d.policy.portAllowed(uint16(n)) {
		return nil, fmt.Errorf("%w: port %s is not permitted", ErrBadURL, portText)
	}
	port := uint16(n)

	// One deadline over the lookup and the connection together, so a resolver
	// that stalls spends the same budget a silent address would.
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	// Every address, not the first acceptable one. A name that answers with
	// one public address and one private one is not half trustworthy: which
	// of them a connection lands on would be decided by ordering and luck,
	// and the name's owner chose to put the private one there.
	for _, addr := range addrs {
		if err := checkAddr(addr, d.policy.relaxed); err != nil {
			return nil, fmt.Errorf("%w (%s resolves to it)", err, host)
		}
	}

	if len(addrs) > maxDialAttempts {
		addrs = addrs[:maxDialAttempts]
	}
	var last error
	for _, addr := range addrs {
		// The literal, never the name. This is the line that makes rebinding
		// impossible: there is no second lookup for a second answer to arrive
		// in, because nothing below this point knows what the host was called.
		conn, err := d.dial(ctx, "tcp", netip.AddrPortFrom(addr, port).String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, fmt.Errorf("egress: connect to %s: %w", host, last)
}

// resolve returns the addresses of a host, looking a name up exactly once.
func (d *dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}

	found, err := d.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoAddress, host)
	}
	addrs := make([]netip.Addr, 0, len(found))
	for _, addr := range found {
		addrs = append(addrs, addr.Unmap())
	}
	return addrs, nil
}

// control re-checks the address at the socket, immediately before connect.
//
// Redundant by design. dialContext has already checked this address, and this
// runs on the number the kernel is about to be given, below anything that could
// have substituted another. It costs one comparison per connection and it is
// the check that still holds if the code above it is one day rearranged.
func (d *dialer) control(_, address string, _ syscall.RawConn) error {
	target, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrForbiddenAddress, address)
	}
	return checkAddr(target.Addr(), d.policy.relaxed)
}

// LimitBody returns a reader that fails once more than limit bytes are read.
//
// For a body consumed as a stream. The error is ErrBodyTooLarge, returned in
// place of the read that crossed the line, so a decoder reading from it stops
// with a failure rather than with a short document it believes is whole.
func LimitBody(r io.Reader, limit int64) io.Reader {
	return &capped{r: r, remaining: limit}
}

// ReadBody reads a whole body, or fails if it is larger than limit.
//
// The limit applies to the bytes as delivered, after any transfer decoding, so
// a small compressed response that expands to a large one is stopped at the
// same line as a large one sent plainly.
func ReadBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(LimitBody(r, limit))
}

type capped struct {
	r         io.Reader
	remaining int64
}

func (c *capped) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.remaining <= 0 {
		// Nothing left in the budget. Whether that is an overrun or an exact
		// fit depends on whether the source has anything more to say, so it
		// is asked for one byte.
		var probe [1]byte
		for {
			n, err := c.r.Read(probe[:])
			if n > 0 {
				return 0, ErrBodyTooLarge
			}
			if err != nil {
				return 0, err
			}
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}
