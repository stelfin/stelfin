package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ServerConfig wires the HTTP surface.
type ServerConfig struct {
	// BaseURL is where the confirmation page is served from.
	BaseURL string
	// Messenger delivers replies over WhatsApp.
	Messenger Messenger
	// AppSecret verifies webhook signatures.
	AppSecret []byte
	// VerifyToken answers Meta's subscription challenge.
	VerifyToken string
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
	// Assets serves the confirmation page. Nil serves no page.
	Assets http.Handler
	// Logger receives request-scoped logs. Nil uses the default.
	Logger *slog.Logger
}

// Server exposes the service over HTTP.
type Server struct {
	svc          *Service
	tokens       *ConfirmTokens
	enrollTokens *EnrollTokens
	cfg          ServerConfig
	log          *slog.Logger
}

// NewServer returns a Server.
func NewServer(svc *Service, tokens *ConfirmTokens, enrollTokens *EnrollTokens, cfg ServerConfig) (*Server, error) {
	switch {
	case svc == nil:
		return nil, errors.New("api: service is required")
	case tokens == nil:
		return nil, errors.New("api: confirmation tokens are required")
	case enrollTokens == nil:
		return nil, errors.New("api: enroll tokens are required")
	case len(cfg.AppSecret) == 0:
		return nil, errors.New("api: webhook app secret is required")
	case cfg.VerifyToken == "":
		return nil, errors.New("api: webhook verify token is required")
	case cfg.TreasuryAddress == "":
		return nil, errors.New("api: treasury address is required")
	case cfg.SignFeeBump == nil:
		return nil, errors.New("api: fee-bump signer is required")
	case cfg.SignProvision == nil:
		return nil, errors.New("api: provisioning signer is required")
	case cfg.BaseURL == "":
		return nil, errors.New("api: base url is required")
	case cfg.Messenger == nil:
		return nil, errors.New("api: messenger is required")
	case cfg.NetworkPassphrase == "":
		return nil, errors.New("api: network passphrase is required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, tokens: tokens, enrollTokens: enrollTokens, cfg: cfg, log: log}, nil
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webhook/whatsapp", s.handleChallenge)
	mux.HandleFunc("POST /webhook/whatsapp", s.handleInbound)
	mux.HandleFunc("GET /v1/confirm", s.handleConfirm)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.HandleFunc("POST /v1/enroll/submit", s.handleEnrollSubmit)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
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
		mux.Handle("GET /static/", s.cfg.Assets)
	}
	return mux
}

// marketingURL is the product's public front door — a Next.js app deployed
// separately, not embedded in this binary. Not config-driven: unlike
// STELFIN_BASE_URL, getting this wrong costs a dead redirect, not a payment
// authorised against the wrong origin, so a constant is enough.
const marketingURL = "https://stelfin.vercel.app"

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	challenge, err := VerifyChallenge(s.cfg.VerifyToken, r.URL.Query())
	if err != nil {
		// Deliberately terse: an unauthenticated caller learns only that it
		// failed, not which check it failed.
		s.log.Warn("webhook challenge refused", "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(challenge))
}

// handleInbound accepts a verified webhook delivery.
//
// It acknowledges immediately and does the work afterwards. Meta retries on a
// slow or failed response, so processing inline would turn one message into
// several — and a payment flow is not something to run more than once per
// message.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	body, err := ReadVerifiedBody(s.cfg.AppSecret, r)
	if err != nil {
		s.log.Warn("webhook delivery refused", "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	messages, err := ParseInbound(body)
	if err != nil {
		// The signature was valid, so this came from Meta — a shape we cannot
		// read is our problem to fix, not a request to reject. Acknowledge so
		// Meta stops retrying something a retry will not fix.
		s.log.Error("unparseable webhook payload", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Acknowledge before doing the work. Meta retries a slow response, and a
	// retry would start the same payment flow again.
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// The request context is cancelled once the handler returns, so the work
	// gets its own with a bound of its own.
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), inboundTimeout)
		defer cancel()

		for _, m := range messages {
			if err := s.svc.HandleInbound(ctx, m, s.cfg.Messenger, s); err != nil {
				// Logged, not retried: the message is already claimed, and
				// replaying it would risk a second confirmation.
				s.log.Error("inbound message failed", "message_id", m.ID, "error", err)
			}
		}
	}()
}

// inboundTimeout bounds work that outlives the request it arrived on.
const inboundTimeout = 60 * time.Second

// handleConfirm returns what the user is being asked to approve.
//
// Authority comes from the confirmation token in the Authorization header, and
// it names exactly one transaction: a token cannot be used to read a different
// payment even for the same user.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	ownerRef, hash, ok := s.authorise(w, r)
	if !ok {
		return
	}

	confirmation, err := s.svc.LoadConfirmation(r.Context(), ownerRef, hash)
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
	ownerRef, hash, ok := s.authorise(w, r)
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

	res, err := s.svc.Submit(r.Context(), ownerRef, req.SignedXDR,
		s.cfg.TreasuryAddress, s.cfg.SignFeeBump)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// The token named one transaction; Submit independently authorised the
	// envelope against pending_sends. Both agreeing is the expected case —
	// a disagreement means a token was reused against a different envelope.
	if res.Hash != "" && hash != "" && !res.AlreadyKnown && res.Hash != hash {
		s.log.Warn("submitted hash differs from the token's",
			"token_hash", hash, "submitted_hash", res.Hash, "owner", ownerRef)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"hash":          res.Hash,
		"ledger":        res.Ledger,
		"already_known": res.AlreadyKnown,
	})
}

// authorise verifies the confirmation token and reports what it grants.
func (s *Server) authorise(w http.ResponseWriter, r *http.Request) (ownerRef, hash string, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	ownerRef, hash, err := s.tokens.Verify(token)
	if err != nil {
		// Invalid and expired are both 401 to the caller; the distinction is
		// only in the log.
		s.log.Warn("confirmation token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	return ownerRef, hash, true
}

type enrollRequest struct {
	Address string `json:"address"`
}

// handleEnroll builds the provisioning transaction for a device-generated
// address and returns it for the device to sign.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ownerRef, ok := s.authoriseEnroll(w, r)
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

	enrollment, err := s.svc.PrepareEnrollment(r.Context(), ownerRef, req.Address, s.cfg.TreasuryAddress)
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
	ownerRef, ok := s.authoriseEnroll(w, r)
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

	res, err := s.svc.SubmitEnrollment(r.Context(), ownerRef, req.SignedXDR,
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

// authoriseEnroll verifies the enroll token and reports the phone number it
// authorises.
func (s *Server) authoriseEnroll(w http.ResponseWriter, r *http.Request) (ownerRef string, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	ownerRef, err := s.enrollTokens.Verify(token)
	if err != nil {
		s.log.Warn("enroll token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return ownerRef, true
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
	case errors.Is(err, ErrAlreadyEnrolled):
		http.Error(w, "already enrolled", http.StatusConflict)
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

// IssueConfirmLink mints the URL sent to a user over WhatsApp.
//
// The token goes in the fragment, not the query string: fragments are not sent
// to the server on page load and do not appear in access logs, proxy logs, or
// Referer headers when the page links out.
func (s *Server) IssueConfirmLink(ownerRef, hash string, expiresAt time.Time) (string, error) {
	token, err := s.tokens.Issue(ownerRef, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/confirm#" + token, nil
}

// IssueEnrollLink mints the URL sent to a not-yet-enrolled user over
// WhatsApp. Same fragment placement, same reasoning as IssueConfirmLink.
func (s *Server) IssueEnrollLink(ownerRef string, expiresAt time.Time) (string, error) {
	token, err := s.enrollTokens.Issue(ownerRef, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/enroll#" + token, nil
}
