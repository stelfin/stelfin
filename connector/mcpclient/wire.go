package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/stelfin/stelfin/connector/egress"
)

// The wire: JSON-RPC 2.0 over MCP's streamable HTTP transport, as a client.
//
// Every request is one POST. The answer is either a JSON body holding one
// response, or an event stream on which the response eventually arrives; both
// are accepted, because a conforming server may choose either. Nothing else of
// the transport is implemented, and the omissions are the point:
//
//   - No GET stream. That is the channel a server uses to speak first, and this
//     client has nothing to say to a server that speaks first.
//   - No resumption, no batches, no stdio.
//   - A request from the server — sampling, elicitation, roots, or anything
//     else carrying a method and an id — ends the call with ErrServerRequest.
//     It is not answered, not even with "method not found": answering is
//     taking part.

// offeredVersion is the protocol revision this client asks for, and
// acceptedVersions are the ones it will proceed under. The three differ in
// features this client does not use; what it does use — initialize, tools/list
// and tools/call with text content — is the same in each.
const offeredVersion = "2025-06-18"

var acceptedVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// endpoint is one remote server and what it takes to speak to it.
type endpoint struct {
	url  string
	http *egress.Client
	// authorise adds the credential to a request.
	//
	// A function rather than a field holding the bytes. A struct printed with
	// %+v shows every field it has, at any depth, and a function prints as an
	// address — so there is no verb, no log line and no panic message through
	// which the credential in here can be rendered.
	authorise func(*http.Request)
	maxBytes  int64
}

// session is one conversation with a server: initialise, then a few calls.
//
// One per Read, and discarded after. A session kept between reads would be
// state a server could shape over time; a fresh one means each read sees the
// server as a stranger would.
type session struct {
	endpoint *endpoint
	// id is the session identifier the server issued, if it issued one.
	id string
	// version is the protocol revision agreed in initialize.
	version string
	next    int
}

// message is one JSON-RPC message as received.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// initialise opens the session.
func (s *session) initialise(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	header, err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": offeredVersion,
		// Empty, and it stays empty. Each capability a client declares is
		// something it has agreed to do when the server asks: run a prompt
		// through a model, put a question to a person, list its files. This
		// client agrees to none of them.
		"capabilities": map[string]any{},
		"clientInfo":   map[string]any{"name": "stelfin", "version": "1"},
	}, &result)
	if err != nil {
		return err
	}
	if !acceptedVersions[result.ProtocolVersion] {
		return fmt.Errorf("%w: it speaks a protocol version this does not", ErrProtocol)
	}
	s.version = result.ProtocolVersion

	if id := header.Get("Mcp-Session-Id"); id != "" {
		// Echoed back to the server that issued it and to nobody else, but it
		// is still the server's text going into a header, so it is held to
		// what the specification allows: visible ASCII.
		if !visibleASCII(id) || len(id) > 512 {
			return fmt.Errorf("%w: its session id is not one", ErrProtocol)
		}
		s.id = id
	}

	return s.notify(ctx, "notifications/initialized")
}

func visibleASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// call sends one request and decodes its result.
func (s *session) call(ctx context.Context, method string, params, out any) (http.Header, error) {
	s.next++
	id := s.next

	resp, err := s.post(ctx, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The number and not the reason phrase or the body: both are the
		// server's text, and this error is on its way to a chat.
		return nil, fmt.Errorf("mcpclient: the server answered %s with HTTP %d",
			method, resp.StatusCode)
	}

	answer, err := s.answer(resp, id)
	if err != nil {
		if errors.Is(err, egress.ErrBodyTooLarge) || errors.Is(err, ErrProtocol) ||
			errors.Is(err, ErrServerRequest) {
			return nil, err
		}
		return nil, fmt.Errorf("mcpclient: reading the answer to %s: %w", method, err)
	}

	if answer.Error != nil {
		// The code and not the message, for the same reason as above. A
		// JSON-RPC error message is free text from the far side.
		return nil, fmt.Errorf("%w: %s was refused with code %d",
			ErrProtocol, method, answer.Error.Code)
	}
	if len(answer.Result) == 0 {
		return nil, fmt.Errorf("%w: the answer to %s has neither a result nor an error",
			ErrProtocol, method)
	}

	decoder := json.NewDecoder(bytes.NewReader(answer.Result))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return nil, fmt.Errorf("%w: the result of %s is not the shape it should be",
			ErrProtocol, method)
	}
	return resp.Header, nil
}

// notify sends a notification, which has no id and expects no answer.
func (s *session) notify(ctx context.Context, method string) error {
	resp, err := s.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mcpclient: the server answered %s with HTTP %d",
			method, resp.StatusCode)
	}
	// Whatever a server says in reply to a notification is not read. There is
	// nothing it could say that this client would act on.
	return nil
}

func (s *session) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.endpoint.url, bytes.NewReader(encoded))
	if err != nil {
		return nil, errors.New("mcpclient: the request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s.id != "" {
		req.Header.Set("Mcp-Session-Id", s.id)
	}
	if s.version != "" {
		req.Header.Set("MCP-Protocol-Version", s.version)
	}
	s.endpoint.authorise(req)

	resp, err := s.endpoint.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: %w", err)
	}
	return resp, nil
}

// answer reads the response to request id out of an HTTP response.
func (s *session) answer(resp *http.Response, id int) (message, error) {
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return message{}, fmt.Errorf("%w: the answer has no content type", ErrProtocol)
	}

	switch kind {
	case "application/json":
		body, err := egress.ReadBody(resp.Body, s.endpoint.maxBytes)
		if err != nil {
			return message{}, err
		}
		msg, err := parseMessage(body)
		if err != nil {
			return message{}, err
		}
		if msg.Method != "" {
			return message{}, refuseServerMessage(msg)
		}
		return msg, checkID(msg, id)

	case "text/event-stream":
		return readStream(egress.LimitBody(resp.Body, s.endpoint.maxBytes), id)

	default:
		return message{}, fmt.Errorf("%w: the answer is neither JSON nor an event stream",
			ErrProtocol)
	}
}

// parseMessage decodes one JSON-RPC message, refusing a batch.
func parseMessage(raw []byte) (message, error) {
	var msg message
	// Unmarshal rather than a streaming decoder: it refuses trailing data, so
	// a body holding one well-formed message followed by something else is
	// malformed rather than half-read.
	if err := json.Unmarshal(raw, &msg); err != nil {
		return message{}, fmt.Errorf("%w: the answer is not a JSON-RPC message", ErrProtocol)
	}
	if msg.JSONRPC != "2.0" {
		return message{}, fmt.Errorf("%w: the answer is not JSON-RPC 2.0", ErrProtocol)
	}
	return msg, nil
}

// refuseServerMessage decides what a message carrying a method means.
func refuseServerMessage(msg message) error {
	if hasID(msg) {
		return fmt.Errorf("%w", ErrServerRequest)
	}
	return fmt.Errorf("%w: a notification arrived where an answer was due", ErrProtocol)
}

func hasID(msg message) bool {
	id := bytes.TrimSpace(msg.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// checkID refuses an answer to a question that was not asked.
//
// A response is matched to its request by id and by nothing else. One carrying
// a different id is either a server confused about which call it is answering
// or one trying to have an old answer read as a new one.
func checkID(msg message, id int) error {
	if !bytes.Equal(bytes.TrimSpace(msg.ID), []byte(strconv.Itoa(id))) {
		return fmt.Errorf("%w: the answer is to a different request", ErrProtocol)
	}
	return nil
}

// readStream reads server-sent events until the response to id arrives.
//
// Notifications on the stream — progress, log lines — are read past and
// dropped. A request on the stream ends the call. The stream is abandoned as
// soon as the response has been read; whatever the server sends after it is
// never seen.
func readStream(body io.Reader, id int) (message, error) {
	reader := bufio.NewReader(body)
	var data []string

	for {
		line, err := reader.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			if err == io.EOF {
				return message{}, fmt.Errorf("%w: the stream ended without an answer", ErrProtocol)
			}
			return message{}, err
		}
		line = strings.TrimRight(line, "\r\n")

		if line != "" {
			// Only data lines carry anything. The event name, the event id
			// and the retry hint are for a client that reconnects, and this
			// one does not.
			if value, found := strings.CutPrefix(line, "data:"); found {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		if len(data) == 0 {
			continue
		}

		msg, err := parseMessage([]byte(strings.Join(data, "\n")))
		data = data[:0]
		if err != nil {
			return message{}, err
		}
		if msg.Method != "" {
			if hasID(msg) {
				return message{}, fmt.Errorf("%w", ErrServerRequest)
			}
			continue
		}
		return msg, checkID(msg, id)
	}
}
