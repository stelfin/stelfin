package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleReclaim returns the envelope that hands an account back.
//
// Authorised by a link token, the same as the SEP-10 pages. The token names one
// owner and one hash, and the service refuses a hash belonging to anyone else —
// this envelope deletes an account, so "which envelope" and "whose" are the two
// things it must not be possible to vary independently.
func (s *Server) handleReclaim(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authoriseLink(w, r)
	if !ok {
		return
	}

	reclaim, err := s.svc.LoadReclaim(r.Context(), scope, hash)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"address":            reclaim.Address,
		"destination":        reclaim.Destination,
		"xdr":                reclaim.XDR,
		"hash":               reclaim.Hash,
		"network_passphrase": reclaim.NetworkPassphrase,
	})
}

type reclaimSubmitRequest struct {
	SignedXDR string `json:"signed_xdr"`
}

// handleReclaimSubmit submits the signed hand-back.
func (s *Server) handleReclaimSubmit(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authoriseLink(w, r)
	if !ok {
		return
	}

	var req reclaimSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	result, err := s.svc.SubmitReclaim(r.Context(), scope, hash, req.SignedXDR)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"address": result.Address,
		"hash":    result.Hash,
		"ledger":  result.Ledger,
	})
}

// IssueReclaimLink mints the URL a member hands their account back through.
//
// A link token rather than a confirm token, and worth saying why: a confirm
// token authorises submitting a payment the operator will fee-bump, and this
// envelope pays its own fee out of the balance it is about to sweep. Reusing
// the payment token would widen what a leaked one can do.
func (s *Server) IssueReclaimLink(scope Scope, hash string, expiresAt time.Time) (string, error) {
	token, err := s.linkTokens.Issue(scope, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/reclaim#" + token, nil
}
