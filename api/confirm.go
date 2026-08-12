package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ErrNoSuchSend reports a hash with no live pending send.
var ErrNoSuchSend = errors.New("api: no pending send with that hash")

// LoadConfirmation rebuilds what a user should be shown for a pending send.
//
// The display is derived from the stored envelope, not from stored display
// strings — the same rule as settlement.Describe, applied one layer out. If a
// future change ever wrote a confirmation row whose numbers disagreed with its
// envelope, this reads the envelope and the disagreement never reaches a user.
//
// ownerRef scopes the lookup: a hash alone is not authority to see a payment.
func (s *Service) LoadConfirmation(ctx context.Context, ownerRef, hash string) (*Confirmation, error) {
	var xdr, toLabel, saidAmount, saidDestination string
	err := s.pool.QueryRow(ctx, `
		SELECT envelope_xdr, to_label, said_amount, said_destination
		  FROM pending_sends
		 WHERE hash = $1 AND owner_ref = $2 AND submitted_at IS NULL AND expires_at > now()`,
		hash, ownerRef,
	).Scan(&xdr, &toLabel, &saidAmount, &saidDestination)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSend, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("api: load pending send %s: %w", hash, err)
	}

	parsed, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		return nil, fmt.Errorf("api: stored envelope for %s is unreadable: %w", hash, err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		return nil, fmt.Errorf("api: stored envelope for %s is not a plain transaction", hash)
	}

	desc, err := s.settle.Describe(tx)
	if err != nil {
		return nil, err
	}
	// The stored hash is what the token authorises; the envelope must still
	// hash to it. A mismatch would mean the row and the envelope disagree
	// about which transaction this is.
	if desc.Hash != hash {
		return nil, fmt.Errorf("api: stored envelope hashes to %s, expected %s", desc.Hash, hash)
	}

	return &Confirmation{
		Amount:          desc.Amount,
		AmountDisplay:   desc.Amount.Display(),
		AssetCode:       desc.AssetCode,
		FromAddress:     desc.From,
		ToAddress:       desc.To,
		ToLabel:         toLabel,
		Hash:            desc.Hash,
		XDR:             xdr,
		SaidAmount:      saidAmount,
		SaidDestination: saidDestination,
	}, nil
}
