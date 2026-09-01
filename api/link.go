package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ledger/store"
)

var (
	// ErrLinkingUnavailable reports a deployment with no web-auth key
	// configured, so no address can be proved.
	ErrLinkingUnavailable = errors.New("api: address linking is not configured")

	// ErrNoChallenge reports a challenge that is unknown, expired or already
	// used. One error for all three: which it was is not information the
	// presenter needs, and telling them narrows a guess.
	ErrNoChallenge = errors.New("api: no live challenge")
)

// LinkChallenge is what the browser is asked to sign.
type LinkChallenge struct {
	// Address is the account being proved.
	Address string
	// XDR is the unsigned challenge. Built with sequence number zero, so it
	// cannot be submitted whatever happens to it afterwards.
	XDR string
	// Hash identifies it.
	Hash string
	// NetworkPassphrase lets the page verify the envelope against the same
	// network the server built it for. A page that guessed would refuse a
	// perfectly good challenge.
	NetworkPassphrase string
	// Purpose is what completing this challenge will do: bind a member's wallet
	// or record a treasury.
	//
	// Sent to the page so it can say which, and read back from our own record
	// rather than from the request when the signature arrives. A client that
	// could choose its own purpose could present one person's wallet proof as
	// proof that a group controls its money.
	Purpose string
}

// PrepareLink issues a challenge proving control of address.
//
// The challenge is bound to the chat identity that asked for it, not only to
// the address. Otherwise anyone who learned a member's public address could
// start a link from their own chat account and, on completing it, inherit that
// member's standing.
func (s *Service) PrepareLink(
	ctx context.Context, scope Scope, identityID store.IdentityID, address string,
) (*LinkChallenge, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if identityID == 0 {
		return nil, errors.New("api: a challenge must be bound to a chat identity")
	}

	challenge, err := s.challenges.Build(address)
	if err != nil {
		return nil, err
	}

	if err := s.store.SaveChallenge(ctx, store.Challenge{
		Hash:      challenge.Hash,
		Org:       scope.Org,
		Identity:  identityID,
		Purpose:   store.PurposeLinkMember,
		Address:   challenge.Address,
		XDR:       challenge.XDR,
		ExpiresAt: challenge.ExpiresAt,
	}); err != nil {
		return nil, err
	}

	return &LinkChallenge{
		Address:           challenge.Address,
		XDR:               challenge.XDR,
		Hash:              challenge.Hash,
		NetworkPassphrase: s.challenges.Network(),
		Purpose:           store.PurposeLinkMember,
	}, nil
}

// LoadChallenge returns a live challenge for signing.
func (s *Service) LoadChallenge(ctx context.Context, scope Scope, hash string) (*LinkChallenge, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}

	c, err := s.store.LiveChallenge(ctx, scope.Org, hash)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}

	return &LinkChallenge{
		Address:           c.Address,
		XDR:               c.XDR,
		Hash:              c.Hash,
		NetworkPassphrase: s.challenges.Network(),
		Purpose:           c.Purpose,
	}, nil
}

// LinkResult describes a completed link.
type LinkResult struct {
	Address string
	Member  store.MemberID
}

// SubmitLink checks a signed challenge and binds the address to the member.
//
// The order is the whole point. The signature is verified against the address
// the challenge was *issued* for, read from our own record — never from what
// came back — so a signer cannot return a challenge for an account they do
// control and have it accepted as proof of one they do not. Only then is the
// challenge spent, and the address bound, in a single transaction.
func (s *Service) SubmitLink(
	ctx context.Context, scope Scope, hash, signedXDR string,
) (*LinkResult, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.challenges == nil {
		return nil, ErrLinkingUnavailable
	}
	if signedXDR == "" {
		return nil, errors.New("api: a signed challenge is required")
	}

	c, err := s.store.LiveChallenge(ctx, scope.Org, hash)
	if errors.Is(err, store.ErrNoChallenge) {
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}
	if err != nil {
		return nil, err
	}

	if c.Purpose != store.PurposeLinkMember {
		// A treasury challenge is not a wallet proof. Redeeming one here would
		// spend the group's proof and bind the treasury's address to whoever
		// presented it, as if it were their personal wallet.
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	}

	if err := s.challenges.VerifyMember(signedXDR, c.Address); err != nil {
		return nil, err
	}

	member, err := s.store.ConsumeChallenge(ctx, scope.Org, hash, c.Address, store.AddressLinked)
	switch {
	case errors.Is(err, store.ErrNoChallenge):
		// Lost a race with another submission of the same signed challenge.
		return nil, fmt.Errorf("%w: %s", ErrNoChallenge, hash)
	case errors.Is(err, store.ErrAddressTaken):
		return nil, err
	case err != nil:
		return nil, err
	}

	return &LinkResult{Address: c.Address, Member: member}, nil
}

// Challenges exposes the challenge issuer, for callers that need to know
// whether linking is available at all.
func (s *Service) Challenges() *identity.Challenges { return s.challenges }
