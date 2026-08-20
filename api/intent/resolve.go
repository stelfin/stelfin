package intent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// Destination resolution turns the user's words into an address.
//
// This is deliberately deterministic and deliberately not the model's job. The
// model may propose that "brother" is a beneficiary label; which address that
// label means is a database lookup. A payment sent to the wrong Stellar
// account cannot be recalled, so a wrong guess here is unrecoverable in a way
// that a wrong guess about an amount is not — the amount at least gets shown
// back for confirmation in a form the user recognises.

var (
	// ErrDestinationNotFound reports a label with no saved recipient.
	ErrDestinationNotFound = errors.New("intent: no saved recipient matches")

	// ErrDestinationAmbiguous reports a label matching more than one saved
	// recipient. Never resolved by picking one: the user is asked.
	ErrDestinationAmbiguous = errors.New("intent: more than one saved recipient matches")

	// ErrDestinationInvalid reports a destination that is not usable as written.
	ErrDestinationInvalid = errors.New("intent: destination is not valid")
)

// Destination is a resolved recipient.
type Destination struct {
	// Address is the Stellar account that will receive the payment.
	Address string
	// Label is what to show the user. For a beneficiary this is the saved
	// label as they wrote it, so the confirmation reads in their own words.
	Label string
	// Kind records how this was resolved, for the audit trail.
	Kind DestinationKind
}

// AmbiguousError carries the candidates so the caller can ask a precise
// question instead of a generic one.
type AmbiguousError struct {
	Label      string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("intent: %q matches %d saved recipients: %s",
		e.Label, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

func (e *AmbiguousError) Unwrap() error { return ErrDestinationAmbiguous }

// Resolver turns a grounded destination into an address.
type Resolver struct {
	pool *pgxpool.Pool
}

// NewResolver returns a Resolver backed by pool.
func NewResolver(pool *pgxpool.Pool) *Resolver { return &Resolver{pool: pool} }

// Resolve maps a verified destination onto an address.
func (r *Resolver) Resolve(ctx context.Context, ownerRef string, g *Grounded) (Destination, error) {
	switch g.DestinationKind {
	case DestinationAddress:
		return resolveAddress(g.DestinationText)
	case DestinationBeneficiary:
		return r.resolveBeneficiary(ctx, ownerRef, g.DestinationText)
	default:
		return Destination{}, fmt.Errorf("%w: unknown kind %q", ErrDestinationInvalid, g.DestinationKind)
	}
}

// resolveAddress validates a raw Stellar address.
//
// strkey checks the checksum, so a transposed or truncated character is
// rejected rather than silently addressing a different account.
func resolveAddress(text string) (Destination, error) {
	address := strings.TrimSpace(text)
	if !strkey.IsValidEd25519PublicKey(address) {
		return Destination{}, fmt.Errorf("%w: %q is not a Stellar address", ErrDestinationInvalid, address)
	}
	return Destination{Address: address, Label: address, Kind: DestinationAddress}, nil
}

// resolveBeneficiary looks a label up among the user's saved recipients.
//
// An exact case-insensitive match wins outright. Failing that, a single
// substring match is accepted so "brother" finds "Brother Chidi". More than one
// match is never resolved by choosing: the user is asked which they meant.
func (r *Resolver) resolveBeneficiary(ctx context.Context, ownerRef, label string) (Destination, error) {
	needle := strings.ToLower(strings.TrimSpace(label))
	if needle == "" {
		return Destination{}, fmt.Errorf("%w: empty label", ErrDestinationInvalid)
	}

	var exactLabel, exactAddress string
	err := r.pool.QueryRow(ctx, `
		SELECT label, address FROM beneficiaries
		 WHERE owner_ref = $1 AND lower(label) = $2`,
		ownerRef, needle,
	).Scan(&exactLabel, &exactAddress)
	if err == nil {
		return Destination{Address: exactAddress, Label: exactLabel, Kind: DestinationBeneficiary}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, fmt.Errorf("intent: look up beneficiary %q: %w", label, err)
	}

	rows, err := r.pool.Query(ctx, `
		SELECT label, address FROM beneficiaries
		 WHERE owner_ref = $1 AND lower(label) LIKE '%' || $2 || '%'
		 ORDER BY label`,
		ownerRef, needle,
	)
	if err != nil {
		return Destination{}, fmt.Errorf("intent: search beneficiaries for %q: %w", label, err)
	}
	defer rows.Close()

	var labels, addresses []string
	for rows.Next() {
		var l, a string
		if err := rows.Scan(&l, &a); err != nil {
			return Destination{}, fmt.Errorf("intent: scan beneficiary: %w", err)
		}
		labels = append(labels, l)
		addresses = append(addresses, a)
	}
	if err := rows.Err(); err != nil {
		return Destination{}, fmt.Errorf("intent: read beneficiaries: %w", err)
	}

	switch len(labels) {
	case 0:
		return Destination{}, fmt.Errorf("%w: %q", ErrDestinationNotFound, label)
	case 1:
		return Destination{Address: addresses[0], Label: labels[0], Kind: DestinationBeneficiary}, nil
	default:
		return Destination{}, &AmbiguousError{Label: label, Candidates: labels}
	}
}
