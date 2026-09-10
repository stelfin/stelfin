// Package mcp exposes a workspace's own data to an agent, and nothing else.
//
// The Model Context Protocol lets somebody point Claude, or an internal agent,
// at their treasury and ask questions about it. That is a genuinely useful
// thing and it is also the single largest new attack surface in this product,
// so the design is mostly a list of things deliberately not built.
//
// # There is no tool that moves money
//
// No stellar_sign, no stellar_submit, no stelfin_execute. An agent can read,
// and at the propose tier it can draft a payment that a human then approves
// through the same screen as every other. The approval is not a formality to be
// automated later: it is the only thing standing between a prompt injection in
// a spreadsheet cell and a treasury.
//
// There is also no stelfin_add_recipient, which looks harmless and is not. An
// agent that can write the address book moves money one indirection later —
// add "payroll" pointing anywhere, wait for somebody to pay payroll.
//
// # HTTP only, never stdio
//
// stdio transport means one process per tenant with that tenant's credentials
// in its environment, spawned by whatever the agent runs on. Multi-tenancy and
// stdio are not compatible in any arrangement worth defending, so this is HTTP
// with a bearer token that names an org.
//
// # Sampling and elicitation are not implemented
//
// Both let a server ask the client's model something. Both are how a compromised
// or merely confused server turns an agent into its own instrument, and neither
// is needed to answer questions about a ledger.
//
// # Every number crosses as a string
//
// A JSON number is a float64 on the other side of the wire, whatever the sender
// meant. A balance rendered as 1000.5000001 and read back as 1000.5000001000001
// is a wrong answer that looks right, so amounts cross as the exact decimal
// text they already are everywhere else in this system.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Protocol version this server implements.
const protocolVersion = "2024-11-05"

// JSON-RPC error codes, as the specification numbers them.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// ErrNotAuthorised reports a request without a live token.
var ErrNotAuthorised = errors.New("mcp: not authorised")

// request is one JSON-RPC call.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is one JSON-RPC reply.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func fail(id json.RawMessage, code int, format string, args ...any) response {
	return response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: fmt.Sprintf(format, args...)},
	}
}

func ok(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

// Tool is one thing an agent may call.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	// Tier is the lowest token tier that may call this.
	Tier string `json:"-"`
	// Run answers the call. It receives already-validated arguments and the
	// org the token named — never an org from the request, which would be a
	// tenancy boundary an agent gets to choose.
	Run func(ctx callContext) (any, error) `json:"-"`
}

// content is one item of a tool result, in MCP's shape.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is what tools/call returns.
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// textResult renders a value as the JSON text an agent reads.
//
// Marshalled here rather than returned as structured content, because the
// numbers inside have already been turned into strings and re-encoding them
// through a generic path is where one would become a float again.
func textResult(v any) (toolResult, error) {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolResult{}, fmt.Errorf("mcp: encode result: %w", err)
	}
	return toolResult{Content: []content{{Type: "text", Text: string(encoded)}}}, nil
}
