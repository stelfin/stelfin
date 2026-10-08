package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ledger/store"
)

// ServerConfig wires the HTTP surface.
type ServerConfig struct {
	// BaseURL is where the confirmation page is served from.
	BaseURL string
	// Handler processes each inbound message: tenancy, the exactly-once claim,
	// role checks and command dispatch.
	//
	// An interface rather than a concrete type because that work lives in core,
	// which imports this package for its service and its tokens. Inverting the
	// dependency here is what keeps the two from importing each other.
	Handler Handler
	// Transports holds the chat platforms this deployment serves. It both
	// routes an inbound webhook to the transport that can authenticate it and
	// delivers every reply — including the refusal to post a link carrying
	// payment authority anywhere bystanders could tap it.
	//
	// A registry with no transports registered is valid: the HTTP surface still
	// serves the signing pages, and /webhook/{channel} answers 404 for
	// everything. That is the state between removing one platform and adding
	// the next.
	Transports *chat.Registry
	// TreasuryAddress pays fees via fee-bump.
	TreasuryAddress string
	// SignFeeBump signs the treasury's outer envelope. It is a function rather
	// than a key so the treasury's signing material can live behind a KMS or
	// HSM without this package ever holding it.
	SignFeeBump func(*txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error)
	// SignProvision signs a provisioning transaction as the treasury. Separate
	// from SignFeeBump because a provisioning transaction is not fee-bumped —
	// the treasury is its source account directly — but the same reasoning
	// applies: a function, not a key, so signing material can live behind a
	// KMS or HSM.
	SignProvision func(*txnbuild.Transaction) (*txnbuild.Transaction, error)
	// NetworkPassphrase is echoed to the confirmation page so it parses the
	// envelope against the same network the server signed for. A page that
	// guessed would fail to verify a perfectly good transaction.
	NetworkPassphrase string
	// Approvals issues and verifies approve tokens.
	//
	// Optional. A deployment without it serves no approval page and refuses
	// /v1/proposal, which is the honest state for one that has not been
	// configured for treasuries rather than a nil dereference on the first
	// person who taps a link.
	Approvals *ApproveTokens
	// Assets serves the confirmation page. Nil serves no page.
	Assets http.Handler
	// Logger receives request-scoped logs. Nil uses the default.
	Logger *slog.Logger
}

// Handler processes one inbound message.
//
// Implemented by core.Service. Declared here because the HTTP surface is what
// calls it and the two must agree on the shape, not because this package knows
// what routing means.
type Handler interface {
	Handle(ctx context.Context, m chat.Inbound, out Replier, links Linker) error
}

// Server exposes the service over HTTP.
type Server struct {
	svc          *Service
	tokens       *ConfirmTokens
	enrollTokens *EnrollTokens
	linkTokens   *LinkTokens
	cfg          ServerConfig
	log          *slog.Logger
}

// NewServer returns a Server.
func NewServer(
	svc *Service, tokens *ConfirmTokens, enrollTokens *EnrollTokens,
	linkTokens *LinkTokens, cfg ServerConfig,
) (*Server, error) {
	switch {
	case svc == nil:
		return nil, errors.New("api: service is required")
	case tokens == nil:
		return nil, errors.New("api: confirmation tokens are required")
	case enrollTokens == nil:
		return nil, errors.New("api: enroll tokens are required")
	case linkTokens == nil:
		return nil, errors.New("api: link tokens are required")
	case cfg.TreasuryAddress == "":
		return nil, errors.New("api: treasury address is required")
	case cfg.SignFeeBump == nil:
		return nil, errors.New("api: fee-bump signer is required")
	case cfg.SignProvision == nil:
		return nil, errors.New("api: provisioning signer is required")
	case cfg.BaseURL == "":
		return nil, errors.New("api: base url is required")
	case cfg.Transports == nil:
		return nil, errors.New("api: transport registry is required")
	case cfg.Handler == nil:
		return nil, errors.New("api: inbound handler is required")
	case cfg.NetworkPassphrase == "":
		return nil, errors.New("api: network passphrase is required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		svc: svc, tokens: tokens, enrollTokens: enrollTokens,
		linkTokens: linkTokens, cfg: cfg, log: log,
	}, nil
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// One route for every platform. The channel is a path segment resolved by
	// an exact lookup against the registry built at startup, so an unknown
	// segment is a 404 before a single byte of the body is read.
	mux.HandleFunc("POST /webhook/{channel}", s.handleWebhook)
	mux.HandleFunc("GET /v1/confirm", s.handleConfirm)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.HandleFunc("POST /v1/enroll/submit", s.handleEnrollSubmit)
	mux.HandleFunc("GET /v1/link", s.handleLink)
	mux.HandleFunc("POST /v1/link/submit", s.handleLinkSubmit)
	mux.HandleFunc("POST /v1/connector/setup", s.handleConnectorSetup)
	mux.HandleFunc("GET /v1/reclaim", s.handleReclaim)
	mux.HandleFunc("POST /v1/reclaim/submit", s.handleReclaimSubmit)
	mux.HandleFunc("GET /v1/proposal", s.handleProposal)
	mux.HandleFunc("POST /v1/proposal/approve", s.handleApprove)
	mux.HandleFunc("POST /v1/proposal/execute", s.handleExecute)
	// Liveness: the process is up and serving. Deliberately asks nothing of
	// anything else, because this is what a platform's health check and a
	// keep-alive ping hit, and a database having a slow minute is not a reason
	// to have the platform restart a process that is serving signing pages.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Readiness: could this instance do its job right now. For a person or a
	// monitor asking why the bot is quiet.
	mux.HandleFunc("GET /ready", s.handleReady)
	// "/{$}" matches only the exact root path, not a catch-all subtree — the
	// marketing site lives elsewhere now (a separate Next.js app), so this
	// binary's job at "/" is just to send a stray visitor there rather than
	// 404 or serve a stale duplicate landing page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, marketingURL, http.StatusFound)
	})
	if s.cfg.Assets != nil {
		mux.Handle("GET /confirm", s.cfg.Assets)
		mux.Handle("GET /enroll", s.cfg.Assets)
		mux.Handle("GET /link", s.cfg.Assets)
		mux.Handle("GET /approve", s.cfg.Assets)
		mux.Handle("GET /reclaim", s.cfg.Assets)
		mux.Handle("GET /static/", s.cfg.Assets)
	}
	return mux
}

// marketingURL is the product's public front door — a Next.js app deployed
// separately, not embedded in this binary. Not config-driven: unlike
// STELFIN_BASE_URL, getting this wrong costs a dead redirect, not a payment
// authorised against the wrong origin, so a constant is enough.
const marketingURL = "https://stelfin.vercel.app"

// handleWebhook accepts a delivery from one chat platform.
//
// The order of the three steps is the security argument: nothing is parsed that
// was not authenticated, nothing is acted on that was not parsed, and the
// acknowledgement is written before any of the work begins.
//
// Acknowledging first is not an optimisation. Discord declares an interaction
// failed if it has not been answered within three seconds, and Telegram retries
// a slow response — which would start the same payment flow twice.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	channel := chat.Channel(r.PathValue("channel"))
	transport, ok := s.cfg.Transports.Lookup(channel)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := transport.Verify(r)
	if err != nil {
		// Deliberately terse: an unauthenticated caller learns only that it
		// failed, not which check it failed.
		s.log.Warn("webhook delivery refused", "channel", channel, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	delivery, err := transport.Parse(body)
	if err != nil {
		// It authenticated, so it came from the platform — a shape we cannot
		// read is our problem to fix, not a request to reject. Acknowledge so
		// the platform stops retrying something a retry will not fix.
		s.log.Error("unparseable webhook delivery", "channel", channel, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	writeAck(w, delivery.Ack)

	if len(delivery.Messages) == 0 {
		return
	}

	// The request context is cancelled once the handler returns, so the work
	// gets its own with a bound of its own.
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), inboundTimeout)
		defer cancel()

		for _, m := range delivery.Messages {
			s.handleInbound(ctx, channel, m)
		}
	}()
}

// readyTimeout bounds the readiness check. An answer that takes longer than
// this is the answer.
const readyTimeout = 2 * time.Second

// handleReady reports whether the database can be reached.
//
// Says only yes or no. The reason goes to the log: this route is public, and
// a connection error names hosts.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.svc.Ready(ctx); err != nil {
		s.log.Warn("not ready", "error", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleInbound runs one message through the handler and makes sure the person
// who sent it is not left with nothing.
//
// Two things can otherwise happen that look, from the chat, like a bot that is
// not there. The handler can fail before it has said anything. And it can
// panic, which in a goroutine nothing recovers takes the whole process down
// with it: every other conversation in flight, for one message nobody
// anticipated.
func (s *Server) handleInbound(ctx context.Context, channel chat.Channel, m chat.Inbound) {
	out := &answering{next: s.cfg.Transports}

	defer func() {
		if p := recover(); p != nil {
			s.log.Error("inbound message panicked",
				"channel", channel, "dedupe_id", m.DedupeID,
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			s.sayItFailed(ctx, out, m, inboundFailedReply)
		}
	}()

	err := s.cfg.Handler.Handle(ctx, m, out, s)
	if err != nil {
		// Logged, not retried: the message is already claimed, and
		// replaying it would risk a second confirmation.
		s.log.Error("inbound message failed",
			"channel", channel, "dedupe_id", m.DedupeID, "error", err)
	}

	switch {
	case out.answered():
		// The handler spoke for itself.
	case err != nil && owedAnAnswer(m):
		s.sayItFailed(ctx, out, m, inboundFailedReply)
	case m.Conversation.InteractionToken != "":
		// Handled, and deliberately without a word. That is right for a
		// message in a group and wrong for an interaction: the platform has
		// already been told an answer is coming, and shows a spinner until
		// one does.
		s.sayItFailed(ctx, out, m, inboundNothingReply)
	}
}

// owedAnAnswer reports whether somebody is waiting on this message.
//
// A command is addressed to the bot, and so is anything that arrived as an
// interaction. Free text in a group is not necessarily: a failure reading it is
// logged, and nobody is told about a message they may not have meant for us.
func owedAnAnswer(m chat.Inbound) bool {
	return m.Command != "" || m.Conversation.InteractionToken != "" || m.Conversation.IsDM
}

// inboundFailedReply is what somebody is told when their message failed before
// anything could answer it. Nothing of the error goes in it: an error can carry
// addresses, SQL and upstream URLs.
const inboundFailedReply = "Something went wrong on my side, so I stopped. " +
	"Nothing was sent or changed — please try that again in a moment."

// inboundNothingReply closes an interaction that needed no answer.
const inboundNothingReply = "There was nothing for me to do with that."

// inboundReplyTimeout bounds the fallback reply. It gets its own, because the
// work it follows may be exactly what used the first one up.
const inboundReplyTimeout = 10 * time.Second

func (s *Server) sayItFailed(ctx context.Context, out *answering, m chat.Inbound, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboundReplyTimeout)
	defer cancel()
	if err := out.Send(ctx, m.Conversation, m.Actor, chat.Reply{Text: text, Ephemeral: true}); err != nil {
		s.log.Error("could not tell the sender their message failed",
			"dedupe_id", m.DedupeID, "error", err)
	}
}

// answering is a Replier that remembers whether it was used.
//
// It counts an attempt, not a delivery. A reply that failed to send was still
// the handler's answer, and following it with a second message would be two
// attempts to reach somebody the platform will not let us reach.
type answering struct {
	next Replier
	used atomic.Bool
}

func (a *answering) Send(ctx context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error {
	a.used.Store(true)
	return a.next.Send(ctx, to, actor, r)
}

func (a *answering) answered() bool { return a.used.Load() }

// writeAck sends the platform's acknowledgement and flushes it, so the response
// is on the wire before the work starts rather than when the handler returns.
func writeAck(w http.ResponseWriter, ack chat.Ack) {
	status := ack.Status
	if status == 0 {
		status = http.StatusOK
	}
	if ack.ContentType != "" {
		w.Header().Set("Content-Type", ack.ContentType)
	}
	w.WriteHeader(status)
	if len(ack.Body) > 0 {
		_, _ = w.Write(ack.Body)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// inboundTimeout bounds work that outlives the request it arrived on.
const inboundTimeout = 60 * time.Second

// handleConfirm returns what the user is being asked to approve.
//
// Authority comes from the confirmation token in the Authorization header, and
// it names exactly one transaction: a token cannot be used to read a different
// payment even for the same user.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authorise(w, r)
	if !ok {
		return
	}

	confirmation, err := s.svc.LoadConfirmation(r.Context(), scope, hash)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"hash":             confirmation.Hash,
		"xdr":              confirmation.XDR,
		"amount":           confirmation.AmountDisplay,
		"asset":            confirmation.AssetCode,
		"to_address":       confirmation.ToAddress,
		"to_label":         confirmation.ToLabel,
		"from_address":     confirmation.FromAddress,
		"said_amount":      confirmation.SaidAmount,
		"said_destination": confirmation.SaidDestination,
		// The page parses the envelope itself and needs the same network to
		// do it. Sending it here means the page never has to be configured
		// separately from the server it talks to.
		"network_passphrase": s.cfg.NetworkPassphrase,
	})
}

type submitRequest struct {
	SignedXDR string `json:"signed_xdr"`
}

// handleSubmit accepts the signed envelope and sends it.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authorise(w, r)
	if !ok {
		return
	}

	var req submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	res, err := s.svc.Submit(r.Context(), scope, req.SignedXDR,
		s.cfg.TreasuryAddress, s.cfg.SignFeeBump)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// The token named one transaction; Submit independently authorised the
	// envelope against pending_sends. Both agreeing is the expected case —
	// a disagreement means a token was reused against a different envelope.
	//
	// Compared against the payment the user signed, not against what landed.
	// What lands is the operator's fee-bump around it, which has a hash of its
	// own, so comparing that fired this warning on every payment ever sent —
	// and a warning that is always on is one nobody reads on the day it is
	// right.
	if res.InnerHash != "" && hash != "" && res.InnerHash != hash {
		s.log.Warn("submitted envelope is not the one the token named",
			"token_hash", hash, "submitted_hash", res.InnerHash, "owner", scope.OwnerRef)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"hash":          res.Hash,
		"ledger":        res.Ledger,
		"already_known": res.AlreadyKnown,
	})
}

// authorise verifies the confirmation token and reports what it grants.
func (s *Server) authorise(w http.ResponseWriter, r *http.Request) (scope Scope, hash string, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, "", false
	}
	scope, hash, err := s.tokens.Verify(token)
	if err != nil {
		// Invalid and expired are both 401 to the caller; the distinction is
		// only in the log.
		s.log.Warn("confirmation token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, "", false
	}
	return scope, hash, true
}

type enrollRequest struct {
	Address string `json:"address"`
}

// handleEnroll builds the provisioning transaction for a device-generated
// address and returns it for the device to sign.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authoriseEnroll(w, r)
	if !ok {
		return
	}

	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.Address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}

	enrollment, err := s.svc.PrepareEnrollment(r.Context(), scope, req.Address, s.cfg.TreasuryAddress)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"address":            enrollment.Address,
		"xdr":                enrollment.XDR,
		"network_passphrase": enrollment.NetworkPassphrase,
	})
}

type enrollSubmitRequest struct {
	SignedXDR string `json:"signed_xdr"`
}

// handleEnrollSubmit accepts the device-signed provisioning envelope and
// submits it, bringing the account into existence.
func (s *Server) handleEnrollSubmit(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authoriseEnroll(w, r)
	if !ok {
		return
	}

	var req enrollSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	res, err := s.svc.SubmitEnrollment(r.Context(), scope, req.SignedXDR,
		s.cfg.TreasuryAddress, s.cfg.SignProvision)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"address":       res.Address,
		"hash":          res.Hash,
		"ledger":        res.Ledger,
		"already_known": res.AlreadyKnown,
	})
}

// handleLink returns the challenge the browser is asked to sign.
func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authoriseLink(w, r)
	if !ok {
		return
	}

	// A set-up link is not a challenge: there is nothing to sign, only a
	// connector to describe and a credential to take.
	if id, isSetup := setupTarget(hash); isSetup {
		row, err := s.svc.ConnectorForSetup(r.Context(), scope, id)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"purpose": "connector_setup",
			"connector": map[string]any{
				"name": row.Name, "kind": row.Kind, "reaches": row.EndpointHost, "ready": row.Ready,
			},
		})
		return
	}

	challenge, err := s.svc.LoadChallenge(r.Context(), scope, hash)
	if err != nil {
		s.writeError(w, err)
		return
	}
	body := map[string]any{
		"address":            challenge.Address,
		"xdr":                challenge.XDR,
		"network_passphrase": challenge.NetworkPassphrase,
		// The page says which handshake it is showing. It is display only: the
		// server dispatches on its own stored purpose when the signature comes
		// back, so a client that lied about this would change nothing but its
		// own headings.
		"purpose": challenge.Purpose,
	}
	if challenge.Statement != "" {
		// The whole text, for the page to hash and compare with the digest in
		// the envelope. It is what the signature commits to.
		body["statement"] = challenge.Statement
	}
	s.writeJSON(w, http.StatusOK, body)
}

type connectorSetupRequest struct {
	// Endpoint is where the connector is. It may be a secret in itself.
	Endpoint string `json:"endpoint"`
	// Credential is the connector's bearer token. Optional: an endpoint that
	// needs none is set up with an empty one.
	Credential string `json:"credential"`
}

// handleConnectorSetup takes a connector's credential and tries the connector.
//
// The credential is in the request body, over TLS, from the page of the admin
// who registered the connector. It is never in a URL, never in chat, and never
// in a response: what comes back is what the connector offers.
func (s *Server) handleConnectorSetup(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authoriseLink(w, r)
	if !ok {
		return
	}
	id, isSetup := setupTarget(hash)
	if !isSetup {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var req connectorSetupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}

	done, err := s.svc.SetUpConnector(r.Context(), scope, id,
		strings.TrimSpace(req.Endpoint), strings.TrimSpace(req.Credential))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":  done.Connector.Name,
		"kind":  done.Connector.Kind,
		"tools": done.Tools,
		"rows":  done.Rows,
	})
}

type linkSubmitRequest struct {
	SignedXDR string `json:"signed_xdr"`
	// Label names a treasury in later messages. Cosmetic, and ignored for a
	// member link.
	Label string `json:"label"`
}

// handleLinkSubmit accepts the signed challenge and binds the address.
func (s *Server) handleLinkSubmit(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authoriseLink(w, r)
	if !ok {
		return
	}

	var req linkSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	done, err := s.svc.SubmitChallenge(r.Context(), scope, hash, req.SignedXDR, req.Label)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := map[string]any{"purpose": done.Purpose}
	switch {
	case done.Member != nil:
		body["address"] = done.Member.Address
	case done.Treasury != nil:
		body["address"] = done.Treasury.Treasury.Address
		body["signed_by"] = done.Treasury.Signed
		body["threshold"] = done.Treasury.Treasury.Medium
	case done.Grant != nil:
		body["address"] = done.Grant.Treasury.Address
		body["signed_by"] = done.Grant.Grant.SignedBy
		body["threshold"] = done.Grant.Threshold
		body["connector"] = done.Grant.Connector.Name
	}
	s.writeJSON(w, http.StatusOK, body)
}

// authoriseLink verifies the link token and reports what it grants.
func (s *Server) authoriseLink(w http.ResponseWriter, r *http.Request) (scope Scope, hash string, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, "", false
	}
	scope, hash, err := s.linkTokens.Verify(token)
	if err != nil {
		s.log.Warn("link token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, "", false
	}
	return scope, hash, true
}

// IssueLinkLink mints the URL a member proves an address through.
//
// Same fragment placement, same reasoning as the others: the token never
// reaches the server on page load, so it stays out of access logs, proxy logs
// and Referer headers.
func (s *Server) IssueLinkLink(scope Scope, hash string, expiresAt time.Time) (string, error) {
	token, err := s.linkTokens.Issue(scope, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/link#" + token, nil
}

// authoriseEnroll verifies the enroll token and reports the owner it
// authorises.
func (s *Server) authoriseEnroll(w http.ResponseWriter, r *http.Request) (scope Scope, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, false
	}
	scope, err := s.enrollTokens.Verify(token)
	if err != nil {
		s.log.Warn("enroll token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, false
	}
	return scope, true
}

func bearer(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// writeError maps a domain error onto a status without leaking which check
// failed to a caller who should not know.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSuchSend), errors.Is(err, ErrUnknownTransaction), errors.Is(err, ErrNotYours):
		// Not-found and not-yours are the same response: telling a caller that
		// a transaction exists but belongs to someone else is itself a leak.
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrAlreadySubmitted):
		http.Error(w, "already submitted", http.StatusConflict)
	case errors.Is(err, ErrExpired):
		http.Error(w, "expired", http.StatusGone)
	case errors.Is(err, ErrUnsigned):
		http.Error(w, "transaction is not signed", http.StatusBadRequest)
	case errors.Is(err, ErrAlreadyEnrolled), errors.Is(err, store.ErrAlreadyGranted):
		http.Error(w, "already enrolled", http.StatusConflict)
	case errors.Is(err, store.ErrProvisioningOff):
		http.Error(w, "this workspace does not set up wallets", http.StatusForbidden)
	case errors.Is(err, store.ErrTooNew):
		// 425: nothing is wrong with the request except when it was made.
		http.Error(w, "too new to be set up a wallet yet", http.StatusTooEarly)
	case errors.Is(err, store.ErrDailyCapReached), errors.Is(err, store.ErrGlobalCapReached),
		errors.Is(err, store.ErrFloatTooLow):
		http.Error(w, "no more wallets can be set up right now", http.StatusTooManyRequests)
	case errors.Is(err, ErrNoChallenge):
		// Unknown, expired and already-used are one answer on purpose.
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, identity.ErrChallengeFailed), errors.Is(err, identity.ErrInvalidAddress):
		http.Error(w, "the signature does not prove that address", http.StatusBadRequest)
	case errors.Is(err, store.ErrAddressTaken):
		http.Error(w, "that address already belongs to another member", http.StatusConflict)
	case errors.Is(err, store.ErrNoConnector):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrConnectorsUnavailable):
		http.Error(w, "connectors are not available", http.StatusServiceUnavailable)
	case errors.Is(err, ErrConnectorChanged):
		http.Error(w, "the connector changed since this was requested", http.StatusConflict)
	case errors.Is(err, ErrConnectorEndpoint):
		// Its own status, because the page can say something useful: the
		// address itself is the problem, and the person can fix it.
		http.Error(w, "that address cannot be used: it must be https, on the public internet",
			http.StatusBadRequest)
	case errors.Is(err, ErrConnectorFailed),
		errors.Is(err, ErrConnectorNotReady), errors.Is(err, ErrConnectorKind):
		// 422: the request was fine and the connector is the problem. The
		// reason is ours to word — never the connector's own text.
		http.Error(w, "the connector could not be reached or did not answer usefully",
			http.StatusUnprocessableEntity)
	case errors.Is(err, ErrReadOnly):
		http.Error(w, "stelfin is in read-only mode", http.StatusServiceUnavailable)
	case errors.Is(err, ErrContractChanged):
		http.Error(w, "the contract's code has changed since it was allowed", http.StatusConflict)
	case errors.Is(err, ErrContractCallsOff), errors.Is(err, store.ErrContractNotAllowed):
		http.Error(w, "that contract cannot be called from here", http.StatusConflict)
	case errors.Is(err, ErrContractCallFailed):
		http.Error(w, "the contract call failed on chain", http.StatusUnprocessableEntity)
	case errors.Is(err, ErrLinkingUnavailable):
		http.Error(w, "address linking is not available", http.StatusServiceUnavailable)
	case errors.Is(err, store.ErrNoReclaim), errors.Is(err, ErrNothingToReclaim):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrAccountNotEmpty), errors.Is(err, ErrCannotReclaim):
		http.Error(w, "this account cannot be handed back as it stands",
			http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrNoProposal), errors.Is(err, store.ErrNoTreasury):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrProposalClosed), errors.Is(err, store.ErrProposalAlreadyOpen):
		http.Error(w, "this proposal is no longer open", http.StatusConflict)
	case errors.Is(err, ErrNotEnoughSignatures):
		// 409 rather than 400: the request is fine and the state is not yet
		// ready. Retrying it unchanged is exactly the wrong thing to do, and
		// the page says so.
		http.Error(w, "not enough signatures yet", http.StatusConflict)
	case errors.Is(err, ErrSequenceMoved):
		http.Error(w, "the treasury has transacted since this proposal was built",
			http.StatusConflict)
	case errors.As(err, new(*MarketMovedError)):
		// 409, with the proposal untouched: nothing was submitted, and the
		// same press will work when the market is back above the minimum.
		http.Error(w, "the market has moved below this swap's minimum; nothing was submitted",
			http.StatusConflict)
	case errors.As(err, new(*RefusedError)):
		// 422: the request was fine and the network said no. The page cannot
		// word the reason from a status, so it sends the person back to the
		// chat, where /execute says which code it was and what to do about it.
		http.Error(w, "the network refused this transaction", http.StatusUnprocessableEntity)
	case errors.Is(err, ErrWrongEnvelope):
		http.Error(w, "that signature is not for this proposal", http.StatusBadRequest)
	case errors.Is(err, ErrTreasuryUnprovable):
		// 422 rather than 400: the request is well formed and the account is
		// the problem, and the page has something specific to say about it.
		http.Error(w, "this account cannot prove control by signature",
			http.StatusUnprocessableEntity)
	default:
		s.log.Error("request failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("write response", "error", err)
	}
}

// IssueConfirmLink mints the URL a reply carries payment authority through.
//
// The token goes in the fragment, not the query string: fragments are not sent
// to the server on page load and do not appear in access logs, proxy logs, or
// Referer headers when the page links out.
func (s *Server) IssueConfirmLink(scope Scope, hash string, expiresAt time.Time) (string, error) {
	token, err := s.tokens.Issue(scope, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/confirm#" + token, nil
}

// IssueEnrollLink mints the URL sent to a not-yet-enrolled user. Same fragment
// placement, same reasoning as IssueConfirmLink.
func (s *Server) IssueEnrollLink(scope Scope, expiresAt time.Time) (string, error) {
	token, err := s.enrollTokens.Issue(scope, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/enroll#" + token, nil
}
