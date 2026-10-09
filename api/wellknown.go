package api

import (
	"fmt"
	"net/http"
	"strings"
)

// handleStellarTOML serves this deployment's stellar.toml: what it says about
// itself to anything that asks the domain.
//
// It says three things and no more. Which network this is. Which key signs the
// challenges a member is asked to counter-sign when they prove a wallet is
// theirs — so that a wallet shown one, or a person looking at it, can check
// that the key on it is this domain's and not somebody's who copied the page.
// And which account pays fees here, which is public on the network anyway and
// is the one address a stranger might reasonably want to recognise.
//
// It lists no assets, because this deployment issues none, and names no
// anchor services, because it is not one. A file that claimed more than is
// true would be read by software that believes it.
//
// Not served at all without a web-auth key: a file with no signing key in it
// answers the one question it exists for with a blank.
func (s *Server) handleStellarTOML(w http.ResponseWriter, r *http.Request) {
	challenges := s.svc.Challenges()
	if challenges == nil {
		http.NotFound(w, r)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "VERSION = %q\n", "2.0.0")
	fmt.Fprintf(&b, "NETWORK_PASSPHRASE = %q\n", s.cfg.NetworkPassphrase)
	fmt.Fprintf(&b, "SIGNING_KEY = %q\n", challenges.ServerAccount())
	if s.cfg.TreasuryAddress != "" {
		fmt.Fprintf(&b, "ACCOUNTS = [%q]\n", s.cfg.TreasuryAddress)
	}
	b.WriteString("\n[DOCUMENTATION]\n")
	fmt.Fprintf(&b, "ORG_NAME = %q\n", "stelfin")
	fmt.Fprintf(&b, "ORG_URL = %q\n", marketingURL)
	fmt.Fprintf(&b, "ORG_DESCRIPTION = %q\n",
		"A non-custodial DAO treasury bot. The signing key above signs wallet-proof challenges and holds nothing; "+
			"the account listed pays network fees and can move nothing else.")

	// SEP-1 asks for this header: the file is fetched by wallets from other
	// origins, and it is public by definition.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(b.String()))
}
