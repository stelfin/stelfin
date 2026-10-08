package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// The page a member signs a contract call on, and what it is given.
//
// A link to it is the same kind of token as a link to confirm a payment: it
// names one org, one person and one transaction hash, and nothing else. It is
// not a fifth kind of token because it does not need to be. A hash names a
// payment or a call and never both, each is looked up in its own table, and a
// token for one presented where the other is expected finds nothing there.

// IssueCallLink mints the URL one member signs one contract call through.
func (s *Server) IssueCallLink(scope Scope, hash string, expiresAt time.Time) (string, error) {
	token, err := s.tokens.Issue(scope, hash, expiresAt)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/call#" + token, nil
}

// handleCall returns the call a member is being asked to sign.
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authorise(w, r)
	if !ok {
		return
	}
	view, err := s.svc.LoadMemberCall(r.Context(), scope, hash)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, callBody(view, s.cfg.NetworkPassphrase))
}

// callBody is what the page is given.
//
// The envelope and the canonical description go across, as on every other
// signing page, and the page derives the second from the first and refuses if
// they differ. Three further things go across that the page checks for itself
// rather than believes: the treasury and the member this link was issued for,
// which have to be the contract called and the account calling; and the
// classic assets a token might be, each of which the page turns into a
// contract address by its own arithmetic before it will put a name to one.
//
// And one thing goes across that the page cannot check: `says`, which is what
// stelfin told the member in chat. A vote names a proposal by its number, and
// what that proposal does is on the ledger and not in the envelope. The page
// shows it, apart from everything else, and says that it is stelfin's word.
func callBody(view *MemberCallView, networkPassphrase string) map[string]any {
	assets := make([]map[string]string, 0, len(view.Assets))
	for _, a := range view.Assets {
		assets = append(assets, assetHint(a))
	}
	return map[string]any{
		"xdr":                view.Call.XDR,
		"hash":               view.Call.Hash,
		"network_passphrase": networkPassphrase,
		"canonical":          view.Description.Canonical(),
		"treasury":           view.Treasury.Address,
		"treasury_label":     view.Treasury.Label,
		"member":             view.Call.Source,
		"assets":             assets,
		"says":               view.Call.Says,
		"note":               view.Call.Note,
		"expires_at":         view.Call.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

func assetHint(a txnbuild.Asset) map[string]string {
	if a.IsNative() {
		return map[string]string{"code": "XLM", "issuer": ""}
	}
	return map[string]string{"code": a.GetCode(), "issuer": a.GetIssuer()}
}

type callSubmitRequest struct {
	SignedXDR string `json:"signed_xdr"`
}

// handleCallSubmit sends a call its member has signed.
func (s *Server) handleCallSubmit(w http.ResponseWriter, r *http.Request) {
	scope, hash, ok := s.authorise(w, r)
	if !ok {
		return
	}

	var req callSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if req.SignedXDR == "" {
		http.Error(w, "signed_xdr is required", http.StatusBadRequest)
		return
	}

	// The link names one call, and the envelope has to be that call. Checked
	// here as well as by the claim underneath: a link for one of a member's
	// calls must not carry in another, even another of their own.
	if view, err := s.svc.LoadMemberCall(r.Context(), scope, hash); err != nil {
		s.writeError(w, err)
		return
	} else if !sameTransaction(view.Call.XDR, req.SignedXDR, s.cfg.NetworkPassphrase) {
		http.Error(w, "that is not the call this link is for", http.StatusBadRequest)
		return
	}

	res, err := s.svc.SubmitMemberCall(r.Context(), scope, req.SignedXDR,
		s.cfg.TreasuryAddress, s.cfg.SignFeeBump)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"hash":     res.Hash,
		"ledger":   res.Ledger,
		"function": res.Function,
		"proposal": res.Proposal,
	})
}

// sameTransaction reports whether two envelopes carry the same transaction,
// whatever signatures each has.
func sameTransaction(issued, signed, networkPassphrase string) bool {
	hashOf := func(envelope string) (string, bool) {
		parsed, err := txnbuild.TransactionFromXDR(envelope)
		if err != nil {
			return "", false
		}
		tx, ok := parsed.Transaction()
		if !ok {
			return "", false
		}
		hash, err := tx.HashHex(networkPassphrase)
		return hash, err == nil
	}
	a, okA := hashOf(issued)
	b, okB := hashOf(signed)
	return okA && okB && a == b
}
