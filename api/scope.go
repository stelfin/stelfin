package api

import (
	"errors"
	"strings"

	"github.com/stelfin/stelfin/ledger"
)

// Scope is who is acting, and in which org.
//
// It exists because the two facts are never useful apart. An owner reference is
// only meaningful inside an org — the same person can be a member of several,
// with different roles and different balances in each — and a query scoped to
// one without the other is either a leak or a lookup that cannot succeed.
//
// Bundling them means the org cannot be dropped by a call site that only
// happened to need the owner. Every service method takes one of these, and so
// does every token, because a token is what carries this context from a chat
// message to a browser that has no session and no cookie.
type Scope struct {
	Org      ledger.OrgID
	OwnerRef string
}

// Valid reports whether this scope names something.
func (s Scope) Valid() bool {
	return s.Org != 0 && s.OwnerRef != ""
}

// check returns an error describing what is missing, for callers that want to
// refuse rather than silently do nothing.
func (s Scope) check() error {
	switch {
	case s.Org == 0:
		return errors.New("api: scope has no org")
	case s.OwnerRef == "":
		return errors.New("api: scope has no owner")
	case strings.ContainsRune(s.OwnerRef, 0):
		// The token payload is NUL-joined, so a NUL in a field would let one
		// combination of values be re-split into a different one.
		return errors.New("api: owner reference must not contain NUL")
	}
	return nil
}
