package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

// maxBody bounds a request. An agent's request is a few hundred bytes; anything
// larger is a mistake or an attempt, and neither should be read into memory
// before it is refused.
const maxBody = 64 << 10

// Tokens resolves a presented bearer token to a workspace.
type Tokens interface {
	LookupMCPToken(ctx context.Context, presented string) (store.MCPToken, error)
}

// Server answers MCP over HTTP.
type Server struct {
	store  *store.Store
	tokens Tokens
	log    *slog.Logger

	tools map[string]Tool
	names []string
}

// callContext is what a tool is given.
//
// The org comes from the token and is not a parameter. An org an agent could
// name is a tenancy boundary an agent gets to choose, and every read below
// takes it — so this is the one place it can come from.
type callContext struct {
	ctx  context.Context
	org  ledger.OrgID
	tier string
	args map[string]any

	store *store.Store
}

// NewServer returns a Server exposing the read-tier tools.
func NewServer(db *store.Store, tokens Tokens, log *slog.Logger) (*Server, error) {
	if db == nil || tokens == nil {
		return nil, errors.New("mcp: a server needs a store and somewhere to check tokens")
	}
	if log == nil {
		log = slog.Default()
	}

	s := &Server{store: db, tokens: tokens, log: log, tools: map[string]Tool{}}
	for _, tool := range readTools() {
		s.tools[tool.Name] = tool
		s.names = append(s.names, tool.Name)
	}
	sort.Strings(s.names)
	return s, nil
}

// Handler is the HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handle)
	return mux
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	token, err := s.authorise(r)
	if err != nil {
		// One answer for a missing, malformed, revoked and expired token.
		// Telling a caller which narrows a guess and helps nobody who is
		// entitled to be here.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, fail(nil, codeParse, "could not read the request"))
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, fail(nil, codeParse, "malformed JSON"))
		return
	}
	if req.JSONRPC != "2.0" {
		writeJSON(w, fail(req.ID, codeInvalidRequest, "jsonrpc must be 2.0"))
		return
	}

	writeJSON(w, s.dispatch(r.Context(), token, req))
}

// authorise resolves the bearer token.
func (s *Server) authorise(r *http.Request) (store.MCPToken, error) {
	header := r.Header.Get("Authorization")
	presented, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return store.MCPToken{}, ErrNotAuthorised
	}
	token, err := s.tokens.LookupMCPToken(r.Context(), strings.TrimSpace(presented))
	if err != nil {
		return store.MCPToken{}, ErrNotAuthorised
	}
	return token, nil
}

func (s *Server) dispatch(
	ctx context.Context, token store.MCPToken, req request,
) response {
	switch req.Method {
	case "initialize":
		return ok(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			// Tools and nothing else. No sampling, no elicitation, no roots:
			// each of those lets a server ask something of the client's model,
			// and none is needed to answer questions about a ledger.
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "stelfin",
				"version": protocolVersion,
			},
		})

	case "notifications/initialized", "ping":
		return ok(req.ID, map[string]any{})

	case "tools/list":
		return ok(req.ID, map[string]any{"tools": s.visibleTools(token.Tier)})

	case "tools/call":
		return s.call(ctx, token, req)

	default:
		return fail(req.ID, codeMethodNotFound, "no method %q", req.Method)
	}
}

// visibleTools lists what this token may call.
//
// A tool above the token's tier is not listed at all, rather than listed and
// refused. An agent shown a tool it cannot use will try it, and a model told
// "you may not" tends to try again differently.
func (s *Server) visibleTools(tier string) []Tool {
	out := make([]Tool, 0, len(s.names))
	for _, name := range s.names {
		tool := s.tools[name]
		if allows(tier, tool.Tier) {
			out = append(out, tool)
		}
	}
	return out
}

// allows reports whether a token tier reaches a tool tier.
func allows(tokenTier, toolTier string) bool {
	if toolTier == store.TierRead {
		return tokenTier == store.TierRead || tokenTier == store.TierPropose
	}
	return tokenTier == toolTier
}

func (s *Server) call(ctx context.Context, token store.MCPToken, req request) response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fail(req.ID, codeInvalidParams, "malformed parameters")
	}

	tool, known := s.tools[params.Name]
	if !known || !allows(token.Tier, tool.Tier) {
		// A tool the token cannot reach is reported as absent, for the same
		// reason it is not listed: "exists but not for you" is an invitation.
		return fail(req.ID, codeMethodNotFound, "no tool %q", params.Name)
	}

	result, err := tool.Run(callContext{
		ctx: ctx, org: token.Org, tier: token.Tier,
		args: params.Arguments, store: s.store,
	})
	if err != nil {
		// A tool failure is reported to the agent as a tool result rather than
		// a protocol error, which is what MCP asks for — and the message is
		// ours, not the database's. An agent relays what it is told, so an
		// internal error's text is one prompt away from a user's screen.
		s.log.Warn("mcp tool failed", "tool", params.Name, "org", token.Org, "error", err)
		return ok(req.ID, toolResult{
			Content: []content{{Type: "text", Text: userFacing(err)}},
			IsError: true,
		})
	}

	rendered, err := textResult(result)
	if err != nil {
		return fail(req.ID, codeInternal, "could not encode the result")
	}
	return ok(req.ID, rendered)
}

// userFacing decides what an agent is told about a failure.
func userFacing(err error) string {
	var invalid *InvalidArgument
	if errors.As(err, &invalid) {
		// The agent can fix this one, so it is told precisely what is wrong.
		return invalid.Error()
	}
	return "that could not be answered"
}

// InvalidArgument reports an argument an agent can correct.
type InvalidArgument struct{ Message string }

func (e *InvalidArgument) Error() string { return e.Message }

func invalidf(format string, args ...any) error {
	return &InvalidArgument{Message: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	// Nothing here is cacheable and some of it is a workspace's balances.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// The status is already written; there is nothing left to say to the
		// client.
		_ = err
	}
}
