package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ServerConfig wires the HTTP surface.
type ServerConfig struct {
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
	// Logger receives request-scoped logs. Nil uses the default.
	Logger *slog.Logger
}

// Server exposes the service over HTTP.
type Server struct {
	svc    *Service
	tokens *ConfirmTokens
	cfg    ServerConfig
	log    *slog.Logger
}

// NewServer returns a Server.
func NewServer(svc *Service, tokens *ConfirmTokens, cfg ServerConfig) (*Server, error) {
	switch {
	case svc == nil:
		return nil, errors.New("api: service is required")
	case tokens == nil:
		return nil, errors.New("api: confirmation tokens are required")
	case len(cfg.AppSecret) == 0:
		return nil, errors.New("api: webhook app secret is required")
	case cfg.VerifyToken == "":
		return nil, errors.New("api: webhook verify token is required")
	case cfg.TreasuryAddress == "":
		return nil, errors.New("api: treasury address is required")
	case cfg.SignFeeBump == nil:
		return nil, errors.New("api: fee-bump signer is required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, tokens: tokens, cfg: cfg, log: log}, nil
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webhook/whatsapp", s.handleChallenge)
	mux.HandleFunc("POST /webhook/whatsapp", s.handleInbound)
	mux.HandleFunc("GET /v1/confirm", s.handleConfirm)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

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

	// Acknowledge before doing anything with the contents.
	w.WriteHeader(http.StatusOK)

	// The body is verified but its contents are still untrusted user input;
	// everything downstream treats it as such.
	s.log.Info("webhook delivery accepted", "bytes", len(body))
}

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
func (s *Server) IssueConfirmLink(baseURL, ownerRef, hash string, expiresAt time.Time) (string, error) {
	token, err := s.tokens.Issue(ownerRef, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return baseURL + "/confirm#" + token, nil
}
