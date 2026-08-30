// Package api orchestrates a chat message into a payment a user can approve.
//
// The pipeline is deliberately linear and every stage narrows what the next one
// may believe:
//
//	message  → Tokenize        backend-owned positions
//	         → Verify          every model claim grounded in the user's text
//	         → Resolve         a label becomes an address, deterministically
//	         → BuildPayment    an unsigned transaction
//	         → Describe        the confirmation, read back out of that
//	                           transaction rather than out of the request
//
// Nothing the model wrote reaches the transaction. It contributes pointers into
// the user's own message and nothing else.
package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// Decoder turns a conversation into a structured proposal with provenance
// spans. Implementations are untrusted: everything they return is verified
// against the backend's own tokenization before it is believed.
type Decoder interface {
	Decode(ctx context.Context, turns []string) (intent.Decoded, error)
}

var (
	// ErrNotASend reports a message that decoded to something other than a
	// payment.
	ErrNotASend = errors.New("api: message is not a send")

	// ErrNoAccount reports a user with no provisioned Stellar account.
	ErrNoAccount = errors.New("api: user has no stellar account")

	// ErrBuilderMismatch reports a built transaction that does not match the
	// instruction it was built from. This should be impossible; it is checked
	// because the consequence of it happening silently is a user signing
	// something they never saw.
	ErrBuilderMismatch = errors.New("api: built transaction does not match the verified instruction")
)

// Config describes the service.
type Config struct {
	// Asset is what users transact in.
	Asset txnbuild.Asset
	// AssetCode is that asset's display code, checked against what the built
	// transaction actually carries.
	AssetCode string
	// AssetID is the ledger's id for that asset, recorded against pending
	// sends so the approval is auditable alongside the ledger entries.
	AssetID int16
	// Challenges issues and verifies SEP-10 challenges. Optional: a deployment
	// with no web-auth key simply cannot link addresses, and says so, rather
	// than refusing to start.
	Challenges *identity.Challenges
}

// Service prepares payments for approval.
type Service struct {
	pool       *pgxpool.Pool
	decoder    Decoder
	resolver   *intent.Resolver
	settle     *settlement.Client
	store      *store.Store
	challenges *identity.Challenges
	cfg        Config
}

// NewService returns a Service.
func NewService(
	pool *pgxpool.Pool, d Decoder, r *intent.Resolver, s *settlement.Client, cfg Config,
) (*Service, error) {
	if cfg.Asset == nil {
		return nil, errors.New("api: asset is required")
	}
	if cfg.AssetCode == "" {
		return nil, errors.New("api: asset code is required")
	}
	return &Service{
		pool: pool, decoder: d, resolver: r, settle: s,
		store: store.New(pool), challenges: cfg.Challenges, cfg: cfg,
	}, nil
}

// Confirmation is what the user is asked to approve.
//
// Every field describing the payment is derived from the transaction in XDR,
// so a screen rendered from this cannot show something other than what the
// signature will commit to.
type Confirmation struct {
	Amount        money.Stroops
	AmountDisplay string
	AssetCode     string

	FromAddress string
	ToAddress   string
	// ToLabel is how to name the recipient to the user: their saved label, or
	// the address itself when there is nothing friendlier.
	ToLabel string

	// Hash and XDR are the transaction. The client signs the XDR; the hash
	// identifies it afterwards.
	Hash string
	XDR  string

	// SaidAmount and SaidDestination are the user's own words, carried through
	// so the screen can show which phrase produced this. They are display only
	// and never feed the transaction.
	SaidAmount      string
	SaidDestination string
}

// PrepareSend turns a conversation into a payment awaiting the user's signature.
//
// turns holds the conversation in order, most recent last. scope identifies who
// is asking and in which org, and bounds every lookup below it: one member's
// saved recipients are not reachable from another's message, and one org's are
// not reachable from another org's at all.
func (s *Service) PrepareSend(ctx context.Context, scope Scope, turns []string) (*Confirmation, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return nil, errors.New("api: conversation is empty")
	}

	decoded, err := s.decoder.Decode(ctx, turns)
	if err != nil {
		return nil, fmt.Errorf("api: decode: %w", err)
	}

	conversation := make([][]intent.Token, len(turns))
	for i, turn := range turns {
		conversation[i] = intent.Tokenize(turn)
	}

	grounded, err := intent.Verify(conversation, decoded)
	if err != nil {
		return nil, err
	}
	if grounded.Action != intent.ActionSend {
		return nil, fmt.Errorf("%w: decoded as %q", ErrNotASend, grounded.Action)
	}

	destination, err := s.resolver.Resolve(ctx, scope.Org, scope.OwnerRef, grounded)
	if err != nil {
		return nil, err
	}

	from, err := s.stellarAddress(ctx, scope)
	if err != nil {
		return nil, err
	}

	tx, err := s.settle.BuildPayment(ctx, settlement.PaymentRequest{
		From:   from,
		To:     destination.Address,
		Asset:  s.cfg.Asset,
		Amount: grounded.Amount,
	})
	if err != nil {
		return nil, err
	}

	// The confirmation comes out of the transaction, not out of the request
	// that built it.
	// Through the general renderer, so there is one implementation of "what does
	// this transaction do" rather than two that can drift. The bridge is as
	// strict as the old Describe was: a caller asking for the payment must not
	// be handed the first of several while the rest ride along unmentioned.
	described, err := s.settle.DescribeTx(tx)
	if err != nil {
		return nil, err
	}
	desc, ok := described.SinglePayment()
	if !ok {
		return nil, fmt.Errorf("%w: not a single payment", settlement.ErrIndescribable)
	}

	// Belt and braces. Describe already guarantees the screen matches the
	// envelope; this catches a builder that produced the wrong envelope in the
	// first place, which the user would otherwise approve without ever seeing
	// the discrepancy.
	if err := agrees(desc, grounded, destination, s.cfg.AssetCode, from); err != nil {
		return nil, err
	}

	xdr, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode transaction: %w", err)
	}

	confirmation := &Confirmation{
		Amount:          desc.Amount,
		AmountDisplay:   desc.Amount.Display(),
		AssetCode:       desc.AssetCode,
		FromAddress:     desc.From,
		ToAddress:       desc.To,
		ToLabel:         destination.Label,
		Hash:            desc.Hash,
		XDR:             xdr,
		SaidAmount:      grounded.AmountText,
		SaidDestination: grounded.DestinationText,
	}

	// Record it before returning. A confirmation the server has not recorded
	// cannot be submitted later, so issuing one without recording it would
	// hand the user an envelope that is guaranteed to be refused.
	expiresAt := time.Unix(tx.Timebounds().MaxTime, 0).UTC()
	if err := s.recordPending(ctx, scope, confirmation, s.cfg.AssetID, expiresAt); err != nil {
		return nil, err
	}
	return confirmation, nil
}

// agrees checks the built transaction against the instruction it came from.
func agrees(
	desc *settlement.PaymentDescription,
	grounded *intent.Grounded,
	destination intent.Destination,
	assetCode, from string,
) error {
	switch {
	case desc.Amount != grounded.Amount:
		return fmt.Errorf("%w: transaction carries %s, instruction said %s",
			ErrBuilderMismatch, desc.Amount, grounded.Amount)
	case desc.To != destination.Address:
		return fmt.Errorf("%w: transaction pays %s, instruction resolved to %s",
			ErrBuilderMismatch, desc.To, destination.Address)
	case desc.From != from:
		return fmt.Errorf("%w: transaction is sourced from %s, expected %s",
			ErrBuilderMismatch, desc.From, from)
	case desc.AssetCode != assetCode:
		return fmt.Errorf("%w: transaction carries %s, expected %s",
			ErrBuilderMismatch, desc.AssetCode, assetCode)
	}
	return nil
}

// stellarAddress finds the account provisioned for a user.
func (s *Service) stellarAddress(ctx context.Context, scope Scope) (string, error) {
	address, err := s.store.AddressForOwner(ctx, scope.Org, scope.OwnerRef)
	if errors.Is(err, store.ErrAddressNotTracked) {
		return "", fmt.Errorf("%w: %s", ErrNoAccount, scope.OwnerRef)
	}
	if err != nil {
		return "", err
	}
	return address, nil
}
