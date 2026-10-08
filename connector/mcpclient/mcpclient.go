// Package mcpclient reads from somebody else's MCP server, as a connector.
//
// The mcp package is the half where an agent calls stelfin. This is the other
// half, where stelfin calls out — and it is the more dangerous one, because the
// thing on the far end is a program written by a stranger whose answers arrive
// inside the product.
//
// # There is no agent loop, and this package cannot contain one
//
// A tool result is returned to the caller as untrusted text and that is the end
// of it. Nothing here reads a result to decide what to do next. A read makes
// one tool call, the one the caller named before the server said anything, and
// what comes back cannot cause a second.
//
// That is enforced by what this package is able to import rather than by what
// it intends. It does not import the decoder, the router, or anything else that
// turns text into an instruction, and a test reads the import list of every
// file here against an allow-list — so the day somebody wires a result into a
// model, a test fails before a reviewer has to notice.
//
// The server cannot start a conversation either. Sampling, elicitation and
// roots are requests a server makes of a client; this client declares none of
// those capabilities, opens no channel for the server to speak first on, and
// ends the call if a request arrives anyway.
//
// # The tool surface is pinned
//
// A DAO grants a server after reading what it offers. What it offers is the
// server's to change the next morning: the tool that read a spreadsheet can
// become one that also takes a destination, under the same name, with a new
// description. So the approved surface is reduced to a digest — over every
// tool's name, description and input schema — and before every call the server
// is asked for its tools again and the digest recomputed. A server that has
// added, removed or altered anything is not called. It fails with
// ErrSurfaceChanged until a person looks at the new surface and pins that.
//
// The listing is not cached. A cache is a window in which the surface that was
// checked is not the surface that is called, and the cost of closing the window
// is one extra request.
//
// # Read tier only
//
// Client is a connector.Reader and deliberately not a connector.Proposer. A
// remote server's text can be shown to a person. It cannot draft a payment.
//
// # Budgets
//
// One deadline covers a whole read, every response is read through a hard byte
// cap, and a circuit breaker stops calling an endpoint that keeps failing. All
// HTTP goes through connector/egress, so the endpoint's address is resolved
// once, checked, and pinned, and redirects are refused — which is also what
// keeps the bearer credential from being sent anywhere but the host it was
// registered for.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stelfin/stelfin/connector"
	"github.com/stelfin/stelfin/connector/egress"
)

var (
	// ErrSurfaceChanged reports a server whose tools are not the ones that
	// were pinned. Nothing is called on a server in this state.
	ErrSurfaceChanged = errors.New("mcpclient: the server's tools have changed since they were approved")

	// ErrToolNotPinned reports a call to a tool outside the approved surface.
	ErrToolNotPinned = errors.New("mcpclient: that tool is not part of the approved surface")

	// ErrCircuitOpen reports an endpoint that has failed repeatedly and is not
	// being called until its cool-down has passed.
	ErrCircuitOpen = errors.New("mcpclient: the server keeps failing and is not being called for now")

	// ErrBadSelector reports a request that does not name a tool and its
	// arguments.
	ErrBadSelector = errors.New("mcpclient: that is not a tool and its arguments")

	// ErrToolFailed reports a tool that ran and said it failed.
	//
	// The tool's own account of why is not carried. It is free text from the
	// far side, an error is the thing most likely to be pasted straight into a
	// chat, and "the tool failed" loses nothing a person could act on.
	ErrToolFailed = errors.New("mcpclient: the tool reported a failure")

	// ErrProtocol reports a server that did not speak MCP as this client
	// accepts it: a malformed message, an answer to a different request, a
	// content type this does not read, a JSON-RPC error.
	ErrProtocol = errors.New("mcpclient: the server did not answer as the protocol requires")

	// ErrServerRequest reports a server that asked the client for something.
	//
	// Sampling, elicitation and roots are all a server reaching back through
	// the client — to its model, its user, its files. None is implemented, and
	// a server that tries is not one to keep talking to.
	ErrServerRequest = errors.New("mcpclient: the server made a request of the client, which is not permitted")
)

// The budgets a client has when none are given.
const (
	// DefaultCallTimeout bounds one Read, across every request it makes.
	DefaultCallTimeout = 15 * time.Second
	// DefaultMaxResponseBytes bounds each response body.
	DefaultMaxResponseBytes = 1 << 20
	// DefaultFailureThreshold is how many consecutive failures open the
	// breaker.
	DefaultFailureThreshold = 5
	// DefaultCoolDown is how long an open breaker waits before letting one
	// call through.
	DefaultCoolDown = time.Minute
)

// Config describes one remote server and what was approved about it.
type Config struct {
	// ID is the connector id a grant is recorded against.
	ID string
	// Endpoint is the server's MCP URL. It must satisfy HTTP.CheckURL.
	Endpoint string

	// SurfaceDigest is the pin: SurfaceDigest of the tools that were approved.
	SurfaceDigest string
	// Tools are the names of the approved tools.
	//
	// The digest cannot be turned back into names, and Describe has to state
	// the capabilities without a network call, so the names are supplied
	// alongside it. They are not trusted to be right: the first listing whose
	// digest matches is checked against them, and a disagreement is refused.
	Tools []string

	// Credential is the bearer token for the server, or empty for none. It is
	// copied; the caller may clear its own slice afterwards.
	Credential []byte

	// HTTP is the client every request goes through.
	HTTP *egress.Client
	// Now is the clock. Nil means time.Now.
	Now connector.Clock

	// CallTimeout bounds one Read. Zero means DefaultCallTimeout.
	CallTimeout time.Duration
	// MaxResponseBytes bounds each response. Zero means
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// FailureThreshold is the number of consecutive failures that opens the
	// breaker. Zero means DefaultFailureThreshold.
	FailureThreshold int
	// CoolDown is how long the breaker stays open. Zero means DefaultCoolDown.
	CoolDown time.Duration
}

// Client reads from one remote MCP server.
type Client struct {
	id       string
	endpoint *endpoint
	now      connector.Clock

	digest string
	// tools are the pinned names, sorted.
	tools  []string
	pinned map[string]bool

	callTimeout time.Duration
	breaker     *breaker
}

// New returns a Client for one server and one approved surface.
func New(cfg Config) (*Client, error) {
	if cfg.ID == "" {
		return nil, errors.New("mcpclient: a client needs the connector id it is granted under")
	}
	target, err := newEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	if !isDigest(cfg.SurfaceDigest) {
		// No digest means no pin, and a client with no pin would call
		// whatever the server offered that day.
		return nil, errors.New("mcpclient: a client needs the digest of the surface that was approved")
	}
	if len(cfg.Tools) == 0 || len(cfg.Tools) > maxTools {
		return nil, errors.New("mcpclient: a client needs the names of the approved tools")
	}
	pinned := make(map[string]bool, len(cfg.Tools))
	for _, name := range cfg.Tools {
		if !validToolName(name) || pinned[name] {
			return nil, errors.New("mcpclient: the approved tools must have distinct, plain names")
		}
		pinned[name] = true
	}
	tools := append([]string(nil), cfg.Tools...)
	sort.Strings(tools)

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	c := &Client{
		id:          cfg.ID,
		endpoint:    target,
		now:         now,
		digest:      cfg.SurfaceDigest,
		tools:       tools,
		pinned:      pinned,
		callTimeout: cfg.CallTimeout,
		breaker: &breaker{
			threshold: cfg.FailureThreshold,
			coolDown:  cfg.CoolDown,
		},
	}
	if c.callTimeout <= 0 {
		c.callTimeout = DefaultCallTimeout
	}
	if c.breaker.threshold <= 0 {
		c.breaker.threshold = DefaultFailureThreshold
	}
	if c.breaker.coolDown <= 0 {
		c.breaker.coolDown = DefaultCoolDown
	}
	return c, nil
}

func newEndpoint(cfg Config) (*endpoint, error) {
	if cfg.HTTP == nil {
		// Not defaulted. A default here would be a way to reach a server
		// without the egress checks, by leaving a field out.
		return nil, errors.New("mcpclient: a client needs an egress client to make requests through")
	}
	if err := cfg.HTTP.CheckURL(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("mcpclient: endpoint: %w", err)
	}

	authorise := func(*http.Request) {}
	if len(cfg.Credential) > 0 {
		for _, b := range cfg.Credential {
			// A credential with a line break in it would be a second header.
			if b < 0x20 || b == 0x7f {
				return nil, errors.New("mcpclient: the credential contains a control character")
			}
		}
		header := "Bearer " + string(cfg.Credential)
		authorise = func(req *http.Request) { req.Header.Set("Authorization", header) }
	}

	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	return &endpoint{
		url: cfg.Endpoint, http: cfg.HTTP, authorise: authorise, maxBytes: maxBytes,
	}, nil
}

func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

// Describe is what a grant is checked against.
//
// It describes the pin, not the server. The capabilities are one entry per
// approved tool — Capability(name), which is what a grant names to permit that
// tool — and one entry carrying the surface digest itself. So
// connector.SurfaceDigest over this descriptor changes whenever the pinned
// surface does, including when only a schema or a description changed and every
// name stayed the same, and a connector.Guard holding a grant issued against
// one surface refuses a client constructed with another.
//
// The two pins do different jobs. The guard's says this client is configured
// with the surface the DAO approved. Read's says the server is still presenting
// that surface. Describe makes no network call and so cannot speak for the
// second, which is why Read checks it every time.
func (c *Client) Describe() connector.Descriptor {
	capabilities := make([]string, 0, len(c.tools)+1)
	capabilities = append(capabilities, surfacePrefix+c.digest)
	for _, name := range c.tools {
		capabilities = append(capabilities, Capability(name))
	}
	return connector.Descriptor{
		ID:           c.id,
		Kind:         connector.KindReader,
		Capabilities: capabilities,
	}
}

// Read calls one tool and returns its text.
//
// The selector names the tool and its arguments; see ParseSelector.
//
// The result's text content comes back one row per line, one cell per row, in
// the order the server sent it, with no column headings. This package does not
// know whether a tool returned prose, JSON or a table and does not try to find
// out: working out what text means is the job of whatever resolves it against
// stelfin's own tables, and a connector that parsed it first would be a second
// opinion nobody asked for. Content that is not text — images, audio, embedded
// resources — is refused rather than skipped.
func (c *Client) Read(
	ctx context.Context, request connector.Request,
) (connector.Observation, error) {
	tool, arguments, err := ParseSelector(request.Selector)
	if err != nil {
		return connector.Observation{}, err
	}
	// Before anything is sent. A tool outside the approved surface is not a
	// question for the server.
	if !c.pinned[tool] {
		return connector.Observation{}, fmt.Errorf("%w: %s", ErrToolNotPinned, tool)
	}

	done, allowed := c.breaker.enter(c.now())
	if !allowed {
		return connector.Observation{}, ErrCircuitOpen
	}

	bounded, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	text, err := c.call(bounded, tool, arguments)
	done(verdict(ctx, err), c.now())
	if err != nil {
		return connector.Observation{}, err
	}
	return observationOf(text, request.Limit, c.now().UTC()), nil
}

// verdict is what a call's result says about the endpoint.
func verdict(caller context.Context, err error) outcome {
	switch {
	case err == nil, errors.Is(err, ErrToolFailed):
		// A tool that ran and reported failure is a server working as
		// designed. It may be the arguments.
		return succeeded
	case errors.Is(caller.Err(), context.Canceled):
		// The caller went away. That is not evidence about the server.
		return inconclusive
	default:
		return failed
	}
}

// call is one read: open a session, check the surface, call the tool.
func (c *Client) call(ctx context.Context, tool string, arguments []byte) ([]string, error) {
	s := &session{endpoint: c.endpoint}
	if err := s.initialise(ctx); err != nil {
		return nil, err
	}

	tools, err := s.listTools(ctx)
	if err != nil {
		return nil, err
	}
	digest, err := SurfaceDigest(tools)
	if err != nil {
		return nil, err
	}
	// The pin. Everything below this line talks to a server that is, as far
	// as its tool surface goes, the one that was approved.
	if digest != c.digest {
		return nil, fmt.Errorf("%w: %s", ErrSurfaceChanged, c.id)
	}
	if len(tools) != len(c.tools) {
		return nil, fmt.Errorf("%w: %s was configured with tool names that do not match its pin",
			ErrSurfaceChanged, c.id)
	}
	for _, offered := range tools {
		if !c.pinned[offered.Name] {
			return nil, fmt.Errorf("%w: %s was configured with tool names that do not match its pin",
				ErrSurfaceChanged, c.id)
		}
	}

	return s.callTool(ctx, tool, arguments)
}

// listTools asks the server what it offers.
func (s *session) listTools(ctx context.Context) ([]Tool, error) {
	var result struct {
		Tools      []Tool `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	if _, err := s.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	// A paginated listing is refused, not followed. The digest is over the
	// whole surface, and a server that hands it out a page at a time decides
	// how many requests a read costs. One with enough tools to need pages is
	// not one whose surface a DAO read before granting.
	if result.NextCursor != "" {
		return nil, fmt.Errorf("%w: its tool list is paginated, and only a whole one can be pinned",
			ErrProtocol)
	}
	return result.Tools, nil
}

// callTool makes the one tool call a read is for.
func (s *session) callTool(ctx context.Context, tool string, arguments []byte) ([]string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	params := map[string]any{"name": tool, "arguments": rawJSON(arguments)}
	if _, err := s.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, tool)
	}

	texts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if item.Type != "text" {
			// Refused rather than skipped. A result that is half text and
			// half something this cannot carry is not the result with the
			// awkward part removed.
			return nil, fmt.Errorf("%w: %s returned content that is not text", ErrProtocol, tool)
		}
		texts = append(texts, item.Text)
	}
	return texts, nil
}

// rawJSON is bytes that are already JSON, marshalled as themselves.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

// observationOf lays a tool's text out as rows.
func observationOf(texts []string, limit int, readAt time.Time) connector.Observation {
	if limit <= 0 || limit > connector.MaxRows {
		limit = connector.MaxRows
	}

	out := connector.Observation{ReadAt: readAt}
	for _, text := range texts {
		// A final newline ends the last line; it does not begin an empty one.
		text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
		for _, line := range strings.Split(text, "\n") {
			if len(out.Rows) == limit {
				// Reported rather than trimmed silently, as everywhere else a
				// connector cuts something short.
				out.Truncated = true
				return out
			}
			out.Rows = append(out.Rows, []connector.Untrusted[string]{connector.Wrap(line)})
		}
	}
	return out
}

// Discover lists a server's tools and computes the digest that would pin them.
//
// For registration: this is what produces the surface a DAO is shown and the
// digest its grant records. It uses the endpoint, credential, client and
// budgets from cfg and ignores the pin, since there is not one yet. It calls no
// tool.
func Discover(ctx context.Context, cfg Config) ([]Tool, string, error) {
	target, err := newEndpoint(cfg)
	if err != nil {
		return nil, "", err
	}
	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s := &session{endpoint: target}
	if err := s.initialise(ctx); err != nil {
		return nil, "", err
	}
	tools, err := s.listTools(ctx)
	if err != nil {
		return nil, "", err
	}
	digest, err := SurfaceDigest(tools)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, digest, nil
}
