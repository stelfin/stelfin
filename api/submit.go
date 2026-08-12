package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/ezedike-evan/stelfin/internal/money"
)

// Submission is deliberately not "accept any signed envelope and send it".
//
// The treasury pays the fee for every transaction it fee-bumps. An endpoint
// that submitted whatever it was handed would let anyone spend the treasury's
// XLM on transactions stelfin never authored — and would also let a user sign
// something other than what they were shown, since nothing would tie the
// envelope back to a confirmation.
//
// So submission is only accepted for a transaction hash this server issued, to
// the user submitting it. A Stellar transaction hash covers the transaction
// but not its signatures, which is exactly the property needed: a matching
// hash proves the envelope is the one that was displayed, while still allowing
// the signature that was absent when it was shown.

var (
	// ErrUnknownTransaction reports an envelope this server never issued.
	ErrUnknownTransaction = errors.New("api: transaction was not issued by this server")

	// ErrNotYours reports a transaction issued to a different user.
	ErrNotYours = errors.New("api: transaction was issued to a different user")

	// ErrAlreadySubmitted reports a transaction that has already been sent.
	ErrAlreadySubmitted = errors.New("api: transaction has already been submitted")

	// ErrExpired reports a transaction past its time bounds.
	ErrExpired = errors.New("api: transaction has expired")

	// ErrUnsigned reports an envelope carrying no signatures.
	ErrUnsigned = errors.New("api: transaction is not signed")
)

// recordPending stores a built transaction so its later submission can be
// authorised. Called as part of preparing a send.
func (s *Service) recordPending(
	ctx context.Context, ownerRef string, c *Confirmation, asset int16, expiresAt time.Time,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pending_sends
			(hash, owner_ref, envelope_xdr, amount, asset_id, destination,
			 to_label, said_amount, said_destination, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (hash) DO NOTHING`,
		c.Hash, ownerRef, c.XDR, int64(c.Amount), asset, c.ToAddress,
		c.ToLabel, c.SaidAmount, c.SaidDestination, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("api: record pending send %s: %w", c.Hash, err)
	}
	return nil
}

// SubmitResult describes an accepted submission.
type SubmitResult struct {
	Hash   string
	Ledger int32
	// AlreadyKnown reports that the transaction was found on chain rather than
	// accepted by this submission.
	AlreadyKnown bool
}

// Submit sends a transaction the user has signed.
//
// signedXDR must be the envelope this server issued, now carrying the user's
// signature. It is authorised against pending_sends, wrapped in a treasury
// fee-bump so the user pays nothing, and submitted.
func (s *Service) Submit(
	ctx context.Context, ownerRef, signedXDR, treasuryAddress string,
	sign func(*txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error),
) (SubmitResult, error) {
	parsed, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("api: unreadable transaction envelope: %w", err)
	}
	inner, ok := parsed.Transaction()
	if !ok {
		// A fee-bump arriving here would mean the client built its own outer
		// envelope, which is the treasury's job.
		return SubmitResult{}, errors.New("api: expected a plain transaction, not a fee-bump")
	}
	if len(inner.Signatures()) == 0 {
		return SubmitResult{}, ErrUnsigned
	}

	hash, err := inner.HashHex(s.settle.Network())
	if err != nil {
		return SubmitResult{}, fmt.Errorf("api: hash transaction: %w", err)
	}

	if err := s.authorise(ctx, ownerRef, hash); err != nil {
		return SubmitResult{}, err
	}

	bump, err := s.settle.FeeBump(inner, treasuryAddress)
	if err != nil {
		return SubmitResult{}, err
	}
	signedBump, err := sign(bump)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("api: sign fee-bump: %w", err)
	}

	res, err := s.settle.SubmitFeeBump(ctx, signedBump)
	if err != nil {
		// The claim stays, so a retry of a genuinely unresolved submission can
		// be authorised again after the operator clears it. Releasing it here
		// would reopen the double-submit window on an outcome we could not
		// establish.
		return SubmitResult{}, err
	}

	return SubmitResult{Hash: res.Hash, Ledger: res.Ledger, AlreadyKnown: res.AlreadyKnown}, nil
}

// authorise claims a pending send for submission.
//
// The claim is a conditional UPDATE rather than a read followed by a write, so
// two concurrent submissions of the same envelope cannot both pass: exactly one
// updates the row, and the other finds nothing to claim.
func (s *Service) authorise(ctx context.Context, ownerRef, hash string) error {
	var claimed bool
	err := s.pool.QueryRow(ctx, `
		UPDATE pending_sends
		   SET submitted_at = now()
		 WHERE hash = $1
		   AND owner_ref = $2
		   AND submitted_at IS NULL
		   AND expires_at > now()
		RETURNING true`,
		hash, ownerRef,
	).Scan(&claimed)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("api: claim pending send %s: %w", hash, err)
	}

	// Nothing was claimed. Work out why, so the caller can tell a replay from
	// a forgery.
	return s.explainRefusal(ctx, ownerRef, hash)
}

func (s *Service) explainRefusal(ctx context.Context, ownerRef, hash string) error {
	var owner string
	var expiresAt time.Time
	var submittedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT owner_ref, expires_at, submitted_at FROM pending_sends WHERE hash = $1`, hash,
	).Scan(&owner, &expiresAt, &submittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownTransaction, hash)
	}
	if err != nil {
		return fmt.Errorf("api: look up pending send %s: %w", hash, err)
	}

	switch {
	case owner != ownerRef:
		return fmt.Errorf("%w: %s", ErrNotYours, hash)
	case submittedAt != nil:
		return fmt.Errorf("%w: %s at %s", ErrAlreadySubmitted, hash, submittedAt.Format(time.RFC3339))
	default:
		return fmt.Errorf("%w: %s expired at %s", ErrExpired, hash, expiresAt.Format(time.RFC3339))
	}
}

// PendingSend is a transaction awaiting signature.
type PendingSend struct {
	Hash        string
	Amount      money.Stroops
	Destination string
	ExpiresAt   time.Time
}

// Pending lists a user's unsubmitted, unexpired transactions.
func (s *Service) Pending(ctx context.Context, ownerRef string) ([]PendingSend, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT hash, amount, destination, expires_at
		  FROM pending_sends
		 WHERE owner_ref = $1 AND submitted_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`,
		ownerRef,
	)
	if err != nil {
		return nil, fmt.Errorf("api: list pending sends for %s: %w", ownerRef, err)
	}
	defer rows.Close()

	var out []PendingSend
	for rows.Next() {
		var p PendingSend
		var amount int64
		if err := rows.Scan(&p.Hash, &amount, &p.Destination, &p.ExpiresAt); err != nil {
			return nil, fmt.Errorf("api: scan pending send: %w", err)
		}
		p.Amount = money.Stroops(amount)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: read pending sends: %w", err)
	}
	return out, nil
}
