package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
	"github.com/stelfin/stelfin/signer"
)

var (
	// ErrNotEnoughSignatures reports an envelope short of its threshold.
	//
	// Not a failure and not something to retry: the answer changes when a
	// person signs, not when this is called again.
	ErrNotEnoughSignatures = errors.New("api: this proposal does not have enough signatures yet")

	// ErrWrongEnvelope reports a signature over something other than the
	// proposal.
	ErrWrongEnvelope = errors.New("api: that signature is not for this proposal")

	// ErrSequenceMoved reports a treasury that has transacted since the
	// proposal was built.
	//
	// The silent killer of multisig, given its own error because nothing else
	// would explain it. The envelope reserves one specific sequence number, so
	// anything else the treasury submits meanwhile invalidates it permanently —
	// with no event, no signal, and a failure much later that reads as a bug.
	ErrSequenceMoved = errors.New("api: the treasury has transacted since this proposal was built")
)

// ProposePaymentParams describes a payment to put to the treasury's signers.
type ProposePaymentParams struct {
	Treasury    store.TreasuryID
	Destination string
	Amount      money.Stroops
	Memo        string
	CreatedBy   store.MemberID
}

// ProposalView is a proposal and everything a caller needs to talk about it.
type ProposalView struct {
	Proposal store.Proposal
	Treasury store.Treasury

	// Description is the canonical text every approver sees, re-derived from
	// the envelope rather than replayed from storage where it matters — see
	// LoadProposal.
	Description *settlement.TxDescription

	// Have and Need are collected weight against the threshold, as the account
	// reports it now. Signed names who has signed; Missing names who has not.
	Have, Need int32
	Signed     []string
	Missing    []string
}

// Ready reports whether the envelope would be accepted on weight alone.
func (v *ProposalView) Ready() bool { return v.Need > 0 && v.Have >= v.Need }

// ProposePayment builds a treasury payment and opens it for approval.
//
// The envelope's time bounds are the org's proposal window, not
// settlement.DefaultTimeout. Three minutes is right for one person confirming
// on their phone and fatally wrong for five people in three time zones: an
// envelope that expires mid-collection is rebuilt with a new sequence number
// and re-signed from scratch by everyone who already agreed.
func (s *Service) ProposePayment(
	ctx context.Context, scope Scope, p ProposePaymentParams,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if p.Amount <= 0 {
		return nil, errors.New("api: a proposal must move a positive amount")
	}

	org, err := s.store.Org(ctx, scope.Org)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, p.Treasury)
	if err != nil {
		return nil, err
	}
	if treasury.Kind != store.TreasuryClassic {
		return nil, fmt.Errorf("api: %s is a contract account; its authorisation is not signatures",
			treasury.Address)
	}

	tx, err := s.settle.Build(ctx, settlement.BuildRequest{
		Source: treasury.Address,
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: p.Destination,
			Amount:      p.Amount.String(),
			Asset:       s.cfg.Asset,
		}},
		Memo:    memoOf(p.Memo),
		Timeout: org.ProposalTTL,
	})
	if err != nil {
		return nil, err
	}

	// Described before it is stored, so an envelope this deployment cannot
	// fully render never reaches a screen that has to summarise it.
	description, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	canonical := description.Canonical()
	digest := sha256.Sum256([]byte(canonical))

	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash proposal: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode proposal: %w", err)
	}

	proposal, err := s.store.CreateProposal(ctx, store.CreateProposalParams{
		Org:      scope.Org,
		Treasury: treasury.ID,
		Kind:     "payment",
		XDR:      envelope,
		Hash:     hash,
		// SourceSeq is the sequence the envelope reserves: the account was at
		// N when it was built and the transaction is valid only at N+1.
		SourceSeq:   tx.SequenceNumber(),
		Description: canonical,
		Digest:      digest[:],
		CreatedBy:   p.CreatedBy,
		ExpiresAt:   time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	})
	if err != nil {
		return nil, err
	}

	return s.viewOf(ctx, scope, proposal, treasury, tx, description)
}

// LoadProposal returns a proposal with its current signing state.
//
// The description is re-derived from the stored envelope rather than read back
// from the description column. The column is the audit record of what was
// shown; the screen has to show what the signature will actually commit to, and
// deriving it from the artifact under signature is what keeps the two from
// drifting.
func (s *Service) LoadProposal(
	ctx context.Context, scope Scope, id store.ProposalID,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}

	proposal, err := s.store.Proposal(ctx, scope.Org, id)
	if err != nil {
		return nil, err
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, proposal.Treasury)
	if err != nil {
		return nil, err
	}

	tx, err := transactionFrom(proposal.XDR)
	if err != nil {
		return nil, err
	}
	description, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, scope, proposal, treasury, tx, description)
}

// viewOf assembles the signing state, reading the signer set from the network.
//
// Read every time, never from the cache. The cache exists so the bot can name
// people without a round trip per keystroke; deciding whether an envelope is
// authorised from it would mean deciding against an account that may have
// changed, which is exactly what someone being removed would exploit.
func (s *Service) viewOf(
	ctx context.Context, scope Scope, proposal store.Proposal, treasury store.Treasury,
	tx *txnbuild.Transaction, description *settlement.TxDescription,
) (*ProposalView, error) {
	set, err := s.settle.SignerSetOf(ctx, treasury.Address)
	if err != nil {
		return nil, err
	}
	summary := set.Summary(treasury.Address)

	stored, err := s.store.Signatures(ctx, scope.Org, proposal.ID)
	if err != nil {
		return nil, err
	}
	signed, err := rebuild(tx, stored, s.settle.Network(), summary)
	if err != nil {
		return nil, err
	}

	view := &ProposalView{
		Proposal:    proposal,
		Treasury:    treasury,
		Description: description,
		Need:        int32(set.Medium),
	}
	for address, weight := range summary {
		if _, did := signed[address]; did {
			view.Have += weight
			view.Signed = append(view.Signed, address)
		} else {
			view.Missing = append(view.Missing, address)
		}
	}
	// Sorted so two calls with the same state produce the same message, and a
	// channel does not see the roster shuffle every time somebody asks.
	sort.Strings(view.Signed)
	sort.Strings(view.Missing)
	return view, nil
}

// Approve records one approver's signature over a proposal.
//
// What arrives is the whole envelope, and it is checked against the proposal's
// own hash before anything else: a signature over a different transaction is
// the one mistake worth catching precisely, because everything downstream would
// treat it as approval of this one.
func (s *Service) Approve(
	ctx context.Context, scope Scope, id store.ProposalID, signedXDR string, by store.IdentityID,
) (*ProposalView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if signedXDR == "" {
		return nil, errors.New("api: an approval needs a signed envelope")
	}

	proposal, err := s.store.Proposal(ctx, scope.Org, id)
	if err != nil {
		return nil, err
	}
	if proposal.Status != store.ProposalOpen {
		return nil, fmt.Errorf("%w: %d is %s", store.ErrProposalClosed, id, proposal.Status)
	}
	treasury, err := s.store.Treasury(ctx, scope.Org, proposal.Treasury)
	if err != nil {
		return nil, err
	}

	returned, err := transactionFrom(signedXDR)
	if err != nil {
		return nil, err
	}
	returnedHash, err := returned.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash returned envelope: %w", err)
	}
	if returnedHash != proposal.Hash {
		return nil, fmt.Errorf("%w: signed %s, expected %s",
			ErrWrongEnvelope, returnedHash, proposal.Hash)
	}

	set, err := s.settle.SignerSetOf(ctx, treasury.Address)
	if err != nil {
		return nil, err
	}
	summary := set.Summary(treasury.Address)

	// Only signatures from keys currently on the account count. A perfectly
	// valid signature from a key that is not a signer is worth nothing, which
	// is the whole reason the account is asked rather than the envelope
	// counted.
	attributed, err := signer.Attribute(returned.Signatures(), returned,
		s.settle.Network(), signer.Set{Signers: summary, Medium: int32(set.Medium)})
	if err != nil {
		return nil, err
	}
	if len(attributed) == 0 {
		return nil, fmt.Errorf("%w: no signature on it comes from a signer on %s",
			ErrWrongEnvelope, treasury.Address)
	}

	// Stored one row per signer. Rows rather than a mutated envelope, so two
	// people approving in the same second produce two rows and not one lost
	// signature.
	for _, sig := range returned.Signatures() {
		address, ok := ownerOf(sig, attributed, returned, s.settle.Network())
		if !ok {
			continue
		}
		if err := s.store.AddSignature(ctx, scope.Org, id, store.ProposalSignature{
			Signer:    address,
			Signature: sig.Signature,
			Weight:    attributed[address],
			AddedBy:   by,
		}); err != nil {
			return nil, err
		}
	}

	return s.LoadProposal(ctx, scope, id)
}

// Execute rebuilds the envelope from the stored signatures and submits it.
//
// Deliberately callable by anyone who can see the proposal. Collecting the
// signatures is the authorisation; who presses the button afterwards is not,
// and requiring a particular person to be online would make this deployment a
// liveness dependency for somebody else's money.
func (s *Service) ExecuteProposal(
	ctx context.Context, scope Scope, id store.ProposalID, by store.IdentityID,
) (*settlement.Result, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}

	view, err := s.LoadProposal(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if view.Proposal.Status != store.ProposalOpen {
		return nil, fmt.Errorf("%w: %d is %s", store.ErrProposalClosed, id, view.Proposal.Status)
	}
	if !view.Ready() {
		return nil, fmt.Errorf("%w: %d of %d", ErrNotEnoughSignatures, view.Have, view.Need)
	}

	// The sequence guard. The envelope is valid only at SourceSeq, so if the
	// account has moved past it the transaction can never be included — and
	// submitting anyway would return tx_bad_seq, which reads as a bug rather
	// than as "somebody else spent from this account on Tuesday".
	account, err := s.settle.LoadAccount(ctx, view.Treasury.Address)
	if err != nil {
		return nil, err
	}
	current, err := account.GetSequenceNumber()
	if err != nil {
		return nil, fmt.Errorf("api: read treasury sequence: %w", err)
	}
	if current >= view.Proposal.SourceSeq {
		if resolveErr := s.store.ResolveProposal(ctx, scope.Org, id, store.ProposalFailed, "",
			fmt.Sprintf("the treasury's sequence moved to %d; this envelope reserved %d",
				current, view.Proposal.SourceSeq), by); resolveErr != nil {
			return nil, resolveErr
		}
		return nil, fmt.Errorf("%w: it is at %d and this envelope reserved %d",
			ErrSequenceMoved, current, view.Proposal.SourceSeq)
	}

	tx, err := transactionFrom(view.Proposal.XDR)
	if err != nil {
		return nil, err
	}
	stored, err := s.store.Signatures(ctx, scope.Org, id)
	if err != nil {
		return nil, err
	}
	signed, err := applySignatures(tx, stored)
	if err != nil {
		return nil, err
	}

	result, err := s.settle.Submit(ctx, signed)
	if err != nil {
		// An ambiguous submission is not a resolution. The proposal stays open
		// and the next attempt finds the transaction on chain or does not — the
		// alternative, marking it failed, would invite a second envelope for a
		// payment that may already have landed.
		return nil, err
	}

	if err := s.store.ResolveProposal(ctx, scope.Org, id, store.ProposalExecuted,
		result.Hash, "", by); err != nil {
		return nil, err
	}
	return &result, nil
}

// rebuild reports which of the account's signers are already on record for this
// proposal, by verifying the stored signatures rather than trusting the rows.
//
// The rows were written by this service, so this is belt and braces — and the
// belt is what the whole flow rests on. A row whose signature no longer
// verifies (the signer was removed, or the key rotated) must stop counting the
// moment the account says so.
func rebuild(
	tx *txnbuild.Transaction, stored []store.ProposalSignature,
	networkPassphrase string, summary map[string]int32,
) (map[string]int32, error) {
	if len(stored) == 0 {
		return map[string]int32{}, nil
	}
	sigs := make([]xdr.DecoratedSignature, 0, len(stored))
	for _, s := range stored {
		hint, err := hintOf(s.Signer)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, xdr.DecoratedSignature{Hint: hint, Signature: s.Signature})
	}
	return signer.Attribute(sigs, tx, networkPassphrase,
		signer.Set{Signers: summary, Medium: 1})
}

// applySignatures puts the stored signatures back onto the envelope.
func applySignatures(
	tx *txnbuild.Transaction, stored []store.ProposalSignature,
) (*txnbuild.Transaction, error) {
	out := tx
	for _, s := range stored {
		hint, err := hintOf(s.Signer)
		if err != nil {
			return nil, err
		}
		out, err = out.AddSignatureDecorated(xdr.DecoratedSignature{
			Hint: hint, Signature: s.Signature,
		})
		if err != nil {
			return nil, fmt.Errorf("api: attach signature from %s: %w", s.Signer, err)
		}
	}
	return out, nil
}

// transactionFrom parses an envelope and refuses a fee bump.
//
// A fee bump would be a different structure with a different hash, and every
// signature check below is against the inner transaction's hash. Accepting one
// here would compare hashes that cannot match and report it as a wrong
// envelope, which is true but unhelpful.
func transactionFrom(envelopeXDR string) (*txnbuild.Transaction, error) {
	parsed, err := txnbuild.TransactionFromXDR(envelopeXDR)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWrongEnvelope, err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		return nil, fmt.Errorf("%w: it is a fee bump, not a transaction", ErrWrongEnvelope)
	}
	return tx, nil
}

// hintOf is the four-byte signature hint for an address.
//
// Derived rather than stored. A stored hint would be a second, forgeable claim
// about which key produced a signature, and the two could disagree.
func hintOf(address string) (xdr.SignatureHint, error) {
	kp, err := keypair.ParseAddress(address)
	if err != nil {
		return xdr.SignatureHint{}, fmt.Errorf("api: %q is not an account: %w", address, err)
	}
	return xdr.SignatureHint(kp.Hint()), nil
}

// ownerOf reports which attributed signer produced one signature.
//
// The hint alone is not enough — four bytes of a public key collide — so the
// signature is verified against each candidate. Only signers Attribute already
// accepted are considered, so this cannot widen what counts.
func ownerOf(
	sig xdr.DecoratedSignature, attributed map[string]int32,
	env signer.Hasher, networkPassphrase string,
) (string, bool) {
	hash, err := env.Hash(networkPassphrase)
	if err != nil {
		return "", false
	}
	for address := range attributed {
		kp, err := keypair.ParseAddress(address)
		if err != nil {
			continue
		}
		if kp.Hint() != [4]byte(sig.Hint) {
			continue
		}
		if kp.Verify(hash[:], sig.Signature) != nil {
			continue
		}
		return address, true
	}
	return "", false
}

// memoOf turns a member's note into a memo, or nothing.
func memoOf(text string) txnbuild.Memo {
	if text == "" {
		return nil
	}
	// Truncated by rune rather than byte: a 28-byte cut through a multi-byte
	// character produces an invalid memo, and Stellar's limit is 28 bytes.
	runes := []rune(text)
	for len(string(runes)) > 28 {
		runes = runes[:len(runes)-1]
	}
	return txnbuild.MemoText(string(runes))
}
