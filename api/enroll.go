package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/settlement"
)

// Enrollment turns a fresh device-generated keypair into a live, funded-by-
// sponsorship Stellar account, and stelfin's record of which phone number it
// belongs to.
//
// It follows the same two-step shape as a payment: PrepareEnrollment builds
// an unsigned transaction and records it pending the user's signature;
// SubmitEnrollment accepts the signed envelope, submits it, and only then
// creates the account stelfin will recognise. Nothing about a phone number
// having "an account" is true until the chain says so.
//
// Unlike a payment, the treasury is this transaction's own source account —
// it sponsors the reserve and creates the account directly, so there is no
// fee-bump wrapper: the treasury signs the transaction itself, once, at
// submit time.

// ErrAlreadyEnrolled reports a phone number that already has a Stellar
// account. Re-enrolling would either be a no-op or, worse, silently orphan an
// existing account's funds behind a mapping nothing points at any more.
var ErrAlreadyEnrolled = errors.New("api: user already has a stellar account")

// Enrollment is the unsigned provisioning transaction a device must sign.
type Enrollment struct {
	Address           string
	XDR               string
	Hash              string
	NetworkPassphrase string
}

// hasStellarAccount reports whether ownerRef already has a provisioned
// account, without leaking which query failed the way stellarAddress's error
// does — callers here only ever need the boolean.
func (s *Service) hasStellarAccount(ctx context.Context, ownerRef string) (bool, error) {
	_, err := s.stellarAddress(ctx, ownerRef)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNoAccount):
		return false, nil
	default:
		return false, err
	}
}

// PrepareEnrollment builds the sponsored-provisioning transaction for a
// brand-new, device-generated address and records it pending signature.
//
// userAddress is generated client-side and never seen by this server before
// this call — there is nothing to look up, unlike PrepareSend's "from"
// account, because the account does not exist yet. This transaction is what
// brings it into existence.
func (s *Service) PrepareEnrollment(
	ctx context.Context, ownerRef, userAddress, treasuryAddress string,
) (*Enrollment, error) {
	if userAddress == "" {
		return nil, errors.New("api: enrollment needs a user address")
	}

	enrolled, err := s.hasStellarAccount(ctx, ownerRef)
	if err != nil {
		return nil, err
	}
	if enrolled {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyEnrolled, ownerRef)
	}

	trustline, ok := s.cfg.Asset.(txnbuild.CreditAsset)
	if !ok {
		return nil, fmt.Errorf("api: asset %T cannot be used as a trustline", s.cfg.Asset)
	}
	changeTrust, err := trustline.ToChangeTrustAsset()
	if err != nil {
		return nil, fmt.Errorf("api: asset as trustline: %w", err)
	}

	tx, err := s.settle.BuildProvision(ctx, treasuryAddress, settlement.ProvisionRequest{
		UserAddress: userAddress,
		Trustlines:  []txnbuild.ChangeTrustAsset{changeTrust},
	})
	if err != nil {
		return nil, err
	}

	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash provisioning transaction: %w", err)
	}
	xdr, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode provisioning transaction: %w", err)
	}

	expiresAt := time.Unix(tx.Timebounds().MaxTime, 0).UTC()
	if err := s.recordPendingEnrollment(ctx, hash, ownerRef, userAddress, xdr, expiresAt); err != nil {
		return nil, err
	}

	return &Enrollment{
		Address:           userAddress,
		XDR:               xdr,
		Hash:              hash,
		NetworkPassphrase: s.settle.Network(),
	}, nil
}

// recordPendingEnrollment stores the built transaction so its later
// submission can be authorised. A phone number may have only one outstanding
// enrollment: a repeat call (a reloaded enroll page, a regenerated device
// key) supersedes whatever was pending before, rather than accumulating
// orphaned attempts. This is not the same as an idempotent retry — the
// transaction carries a wall-clock timeout, so two calls even moments apart
// generally build different transactions with different hashes — it is
// specifically "the newest attempt wins", enforced by the partial unique
// index on owner_ref.
func (s *Service) recordPendingEnrollment(
	ctx context.Context, hash, ownerRef, address, xdr string, expiresAt time.Time,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pending_enrollments (hash, owner_ref, address, envelope_xdr, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_ref) WHERE submitted_at IS NULL DO UPDATE
		   SET hash = EXCLUDED.hash,
		       address = EXCLUDED.address,
		       envelope_xdr = EXCLUDED.envelope_xdr,
		       created_at = now(),
		       expires_at = EXCLUDED.expires_at`,
		hash, ownerRef, address, xdr, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("api: record pending enrollment %s: %w", hash, err)
	}
	return nil
}

// EnrollResult describes an accepted enrollment.
type EnrollResult struct {
	Address string
	Hash    string
	Ledger  int32
	// AlreadyKnown reports that the transaction was found on chain rather than
	// accepted by this submission.
	AlreadyKnown bool
}

// SubmitEnrollment accepts the user-signed provisioning envelope, submits it,
// and on success creates the ledger account and phone-to-address mapping that
// make the user recognisable to the rest of the system.
//
// signedXDR must be the envelope PrepareEnrollment issued, now carrying the
// user's signature over the operations it sources (ChangeTrust and
// EndSponsoringFutureReserves). The treasury's own signature — required
// because it sources BeginSponsoringFutureReserves and CreateAccount, and is
// the transaction's source account — is added here rather than earlier, the
// same reasoning as the fee-bump signer for sends: signing material stays out
// of the request path until the moment it is actually needed.
//
// Known gap: unlike a send, there is no independent path that later
// reconciles a provisioning transaction that lands on chain but whose
// finalisation step (below) never runs — ingestion only watches Payment
// operations, and CreateAccount/ChangeTrust are not payments. A crash between
// a successful settle.Submit and finalizeEnrollment would leave a live,
// funded account that stelfin's own tables do not know about. Accepted for
// now the same way DESIGN.md accepts other narrow crash windows; worth a
// reconciliation job before this carries real users.
func (s *Service) SubmitEnrollment(
	ctx context.Context, ownerRef, signedXDR, treasuryAddress string,
	sign func(*txnbuild.Transaction) (*txnbuild.Transaction, error),
) (EnrollResult, error) {
	parsed, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("api: unreadable enrollment envelope: %w", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		return EnrollResult{}, errors.New("api: expected a plain transaction, not a fee-bump")
	}
	if len(tx.Signatures()) == 0 {
		return EnrollResult{}, ErrUnsigned
	}

	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return EnrollResult{}, fmt.Errorf("api: hash transaction: %w", err)
	}

	address, err := s.authoriseEnrollment(ctx, ownerRef, hash)
	if err != nil {
		return EnrollResult{}, err
	}

	signedTx, err := sign(tx)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("api: sign provisioning transaction: %w", err)
	}

	res, err := s.settle.Submit(ctx, signedTx)
	if err != nil {
		// The claim stays, mirroring Submit's own reasoning: a retry of a
		// genuinely unresolved submission can be authorised again once the
		// outcome is established, and releasing it here would reopen the
		// double-submit window on an outcome we could not establish.
		return EnrollResult{}, err
	}

	if err := s.finalizeEnrollment(ctx, ownerRef, address); err != nil {
		return EnrollResult{}, err
	}

	return EnrollResult{
		Address: address, Hash: res.Hash, Ledger: res.Ledger, AlreadyKnown: res.AlreadyKnown,
	}, nil
}

// authoriseEnrollment claims a pending enrollment for submission, the same
// conditional-UPDATE shape as Submit's authorise: two concurrent submissions
// of the same envelope cannot both pass.
func (s *Service) authoriseEnrollment(ctx context.Context, ownerRef, hash string) (address string, err error) {
	var claimed string
	err = s.pool.QueryRow(ctx, `
		UPDATE pending_enrollments
		   SET submitted_at = now()
		 WHERE hash = $1
		   AND owner_ref = $2
		   AND submitted_at IS NULL
		   AND expires_at > now()
		RETURNING address`,
		hash, ownerRef,
	).Scan(&claimed)
	if err == nil {
		return claimed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("api: claim pending enrollment %s: %w", hash, err)
	}
	return "", s.explainEnrollRefusal(ctx, ownerRef, hash)
}

// explainEnrollRefusal distinguishes why a claim failed. The same error
// values as Submit's refusal path (ErrUnknownTransaction, ErrNotYours,
// ErrAlreadySubmitted, ErrExpired) apply unchanged: they already name the
// generic "transaction", not specifically a send, and the HTTP layer's status
// mapping for them is correct for enrollment too.
func (s *Service) explainEnrollRefusal(ctx context.Context, ownerRef, hash string) error {
	var owner string
	var expiresAt time.Time
	var submittedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT owner_ref, expires_at, submitted_at FROM pending_enrollments WHERE hash = $1`, hash,
	).Scan(&owner, &expiresAt, &submittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownTransaction, hash)
	}
	if err != nil {
		return fmt.Errorf("api: look up pending enrollment %s: %w", hash, err)
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

// finalizeEnrollment creates the ledger account and the phone-to-address
// mapping that make ownerRef resolvable — by PrepareSend's stellarAddress
// lookup, and by intent.Resolver's send-to-phone-number path — now that the
// chain has confirmed the account is real.
func (s *Service) finalizeEnrollment(ctx context.Context, ownerRef, address string) error {
	accountID, err := s.ledgerStore.EnsureAccount(ctx, ledger.AccountUser, ownerRef, ownerRef)
	if err != nil {
		return err
	}
	// Same statement ingestion.Ingester.Track uses to register an address it
	// should index — enrollment and ingestion converge on the same mapping,
	// deliberately, rather than each owning a different notion of "tracked".
	_, err = s.pool.Exec(ctx, `
		INSERT INTO stellar_accounts (address, ledger_account_id)
		VALUES ($1, $2)
		ON CONFLICT (address) DO NOTHING`,
		address, int64(accountID),
	)
	if err != nil {
		return fmt.Errorf("api: track enrolled address %s: %w", address, err)
	}
	return nil
}
