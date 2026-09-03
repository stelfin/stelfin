package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/stelfin/stelfin/ledger/store"
)

// handleProposal returns what an approver is being asked to sign.
func (s *Server) handleProposal(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := s.authoriseApprove(w, r)
	if !ok {
		return
	}

	view, err := s.svc.LoadProposal(r.Context(), scope, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, proposalBody(view, s.cfg.NetworkPassphrase))
}

type approveRequest struct {
	SignedXDR string `json:"signed_xdr"`
}

// handleApprove records one approver's signature.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := s.authoriseApprove(w, r)
	if !ok {
		return
	}

	var req approveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	// The identity is not taken from the request. Who signed is decided by
	// which key the signature verifies against, and who pressed the button is
	// only an audit note — one the token already carries.
	view, err := s.svc.Approve(r.Context(), scope, id, req.SignedXDR, 0)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, proposalBody(view, s.cfg.NetworkPassphrase))
}

// handleExecute submits a proposal that has reached its threshold.
//
// Reachable by anyone holding an approve link for it, which is the same set of
// people who could have executed it from chat. Collecting the signatures is the
// authorisation; who presses the button afterwards is not.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := s.authoriseApprove(w, r)
	if !ok {
		return
	}

	result, err := s.svc.Execute(r.Context(), scope, id, 0)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"hash":   result.Hash,
		"ledger": result.Ledger,
	})
}

// authoriseApprove verifies the approve token and reports what it grants.
func (s *Server) authoriseApprove(
	w http.ResponseWriter, r *http.Request,
) (scope Scope, id store.ProposalID, ok bool) {
	if s.cfg.Approvals == nil {
		http.Error(w, "approvals are not available", http.StatusServiceUnavailable)
		return Scope{}, 0, false
	}
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, 0, false
	}
	scope, id, err := s.cfg.Approvals.Verify(token)
	if err != nil {
		s.log.Warn("approve token refused", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Scope{}, 0, false
	}
	return scope, id, true
}

// proposalBody is what the approval page is given.
//
// The canonical description goes across as text, and the envelope goes across
// too: the page re-derives the description from the envelope and refuses if the
// two disagree. Sending only the description would ask the page to trust the
// server about what the signature commits to, which is the one thing it must
// not do.
func proposalBody(view *ProposalView, networkPassphrase string) map[string]any {
	return map[string]any{
		"id":                 int64(view.Proposal.ID),
		"xdr":                view.Proposal.XDR,
		"hash":               view.Proposal.Hash,
		"network_passphrase": networkPassphrase,
		"treasury":           view.Treasury.Address,
		"treasury_label":     view.Treasury.Label,
		"status":             view.Proposal.Status,
		"expires_at":         view.Proposal.ExpiresAt.UTC().Format(time.RFC3339),
		"canonical":          view.Description.Canonical(),
		"have":               view.Have,
		"need":               view.Need,
		"signed":             view.Signed,
		"missing":            view.Missing,
		"ready":              view.Ready(),
	}
}

// IssueApproveLink mints the URL one approver signs a proposal through.
//
// Fragment-borne like the others: the token is not sent to the server on page
// load and does not appear in access logs, proxy logs or Referer headers.
func (s *Server) IssueApproveLink(
	scope Scope, id store.ProposalID, expiresAt time.Time,
) (string, error) {
	if s.cfg.Approvals == nil {
		return "", ErrLinkingUnavailable
	}
	token, err := s.cfg.Approvals.Issue(scope, id, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/approve#" + token, nil
}
