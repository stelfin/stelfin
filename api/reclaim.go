package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// ReclaimTimeout is how long a reclaim envelope stays valid.
//
// Longer than a payment's and shorter than a proposal's. One person signs it,
// but they are handing back a wallet rather than confirming a payment they just
// asked for — the thought takes longer than the tap.
const ReclaimTimeout = 20 * time.Minute

var (
	// ErrNothingToReclaim reports an account the operator never paid for.
	//
	// A member's own wallet is theirs to merge or keep, and stelfin building
	// them a transaction that deletes it would be doing something nobody asked
	// for with a key it does not hold.
	ErrNothingToReclaim = errors.New("api: this account was not provisioned by the operator")

	// ErrAccountNotEmpty reports an account holding something a merge would
	// destroy.
	ErrAccountNotEmpty = errors.New("api: this account still holds something")

	// ErrCannotReclaim reports an account a merge cannot be built for at all.
	ErrCannotReclaim = errors.New("api: this account cannot be handed back as it stands")
)

// Reclaim is the unsigned transaction that hands an account back.
type Reclaim struct {
	Address     string
	Destination string
	// Releasing is what the operator gets back: the base reserve plus the
	// trustlines this envelope removes. Reported so the member can see the
	// point of what they are signing.
	Releasing money.Stroops
	XDR       string
	Hash      string
	// NetworkPassphrase lets the page parse against the same network the
	// envelope was built for.
	NetworkPassphrase string
}

// PrepareReclaim builds the transaction that returns a provisioned account.
//
// Every refusal below is the same refusal: an AccountMerge sweeps the account's
// entire XLM balance to the destination and deletes the account, and there is
// no undo. So this builds one only when it can account for everything the
// account holds, and says plainly what is in the way otherwise.
func (s *Service) PrepareReclaim(ctx context.Context, scope Scope, address string) (*Reclaim, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if s.cfg.SponsorAddress == "" {
		return nil, errors.New("api: no sponsor account is configured to reclaim into")
	}
	if address == s.cfg.SponsorAddress {
		return nil, fmt.Errorf("%w: it is the sponsor account", ErrCannotReclaim)
	}

	grant, ok, err := s.store.GrantFor(ctx, scope.Org, address)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNothingToReclaim, address)
	}
	if grant.Reclaimed {
		return nil, fmt.Errorf("%w: %s was already handed back", ErrNothingToReclaim, address)
	}

	// A treasury is not somebody's spare wallet. Refused explicitly rather than
	// left to the grant check, because a workspace that provisioned an account
	// and later made it a treasury would otherwise be one command from having
	// it deleted.
	if _, err := s.store.TreasuryByAddress(ctx, scope.Org, address); err == nil {
		return nil, fmt.Errorf("%w: it is this workspace's treasury", ErrCannotReclaim)
	} else if !errors.Is(err, store.ErrNoTreasury) {
		return nil, err
	}

	account, err := s.settle.LoadAccount(ctx, address)
	if err != nil {
		return nil, err
	}

	// Sub-entries a merge cannot clear. Each is a separate sentence to the
	// member because each has a different fix, and "it cannot be merged" with
	// no reason is a dead end.
	if len(account.Data) > 0 {
		return nil, fmt.Errorf(
			"%w: it has %d data entry(s), which have to be removed first",
			ErrCannotReclaim, len(account.Data))
	}
	if account.NumSponsoring > 0 {
		return nil, fmt.Errorf(
			"%w: it is sponsoring reserves for %d other entry(s)",
			ErrCannotReclaim, account.NumSponsoring)
	}

	var ops []txnbuild.Operation
	release := money.Stroops(0)
	for _, b := range account.Balances {
		if b.Asset.Type == "native" {
			continue
		}
		held, parseErr := money.Parse(b.Balance)
		if parseErr != nil {
			return nil, fmt.Errorf("api: read %s balance: %w", b.Asset.Code, parseErr)
		}
		if held > 0 {
			// Not a technicality. Removing a trustline is what makes the merge
			// possible, and a trustline can only be removed at zero — so the
			// only way this could "work" would be to destroy the balance.
			return nil, fmt.Errorf(
				"%w: %s %s. Send it somewhere first — handing the account back "+
					"would mean giving that up",
				ErrAccountNotEmpty, held, b.Asset.Code)
		}
		asset, assetErr := trustlineAsset(b)
		if assetErr != nil {
			return nil, assetErr
		}
		ops = append(ops, &txnbuild.ChangeTrust{Line: asset, Limit: "0"})
		release += settlement.BaseReserve
	}

	ops = append(ops, &txnbuild.AccountMerge{Destination: s.cfg.SponsorAddress})
	release += settlement.AccountReserve

	tx, err := s.settle.Build(ctx, settlement.BuildRequest{
		Source:     address,
		Operations: ops,
		Timeout:    ReclaimTimeout,
	})
	if err != nil {
		return nil, err
	}

	// Described before it is stored, so an envelope this deployment cannot
	// fully render never reaches a page that has to show what it does.
	if _, err := s.settle.DescribeTx(tx); err != nil {
		return nil, err
	}

	hash, err := tx.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash reclaim: %w", err)
	}
	envelope, err := tx.Base64()
	if err != nil {
		return nil, fmt.Errorf("api: encode reclaim: %w", err)
	}

	if err := s.store.SaveReclaim(ctx, store.Reclaim{
		Hash:        hash,
		Org:         scope.Org,
		OwnerRef:    scope.OwnerRef,
		Address:     address,
		Destination: s.cfg.SponsorAddress,
		XDR:         envelope,
		ExpiresAt:   time.Unix(tx.Timebounds().MaxTime, 0).UTC(),
	}); err != nil {
		return nil, err
	}

	return &Reclaim{
		Address:           address,
		Destination:       s.cfg.SponsorAddress,
		Releasing:         release,
		XDR:               envelope,
		Hash:              hash,
		NetworkPassphrase: s.settle.Network(),
	}, nil
}

// LoadReclaim returns a live reclaim for signing.
func (s *Service) LoadReclaim(ctx context.Context, scope Scope, hash string) (*Reclaim, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	r, err := s.store.LiveReclaim(ctx, scope.Org, hash)
	if err != nil {
		return nil, err
	}
	if r.OwnerRef != scope.OwnerRef {
		// Someone else's envelope. Reported as absent rather than as forbidden:
		// confirming it exists tells a caller something about an account that
		// is not theirs.
		return nil, fmt.Errorf("%w: %s", store.ErrNoReclaim, hash)
	}
	return &Reclaim{
		Address:           r.Address,
		Destination:       r.Destination,
		XDR:               r.XDR,
		Hash:              r.Hash,
		NetworkPassphrase: s.settle.Network(),
	}, nil
}

// ReclaimResult describes a completed hand-back.
type ReclaimResult struct {
	Address string
	Hash    string
	Ledger  int32
}

// SubmitReclaim submits the signed envelope and forgets the account.
//
// The order matters and is the opposite of the intuitive one. The reclaim is
// claimed first, so two submissions of the same signed envelope cannot both
// proceed; the address is released only after the network has accepted the
// merge, because an address forgotten while the account still exists is an
// account nothing is watching.
func (s *Service) SubmitReclaim(
	ctx context.Context, scope Scope, hash, signedXDR string,
) (*ReclaimResult, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if signedXDR == "" {
		return nil, errors.New("api: a signed envelope is required")
	}

	stored, err := s.store.LiveReclaim(ctx, scope.Org, hash)
	if err != nil {
		return nil, err
	}
	if stored.OwnerRef != scope.OwnerRef {
		return nil, fmt.Errorf("%w: %s", store.ErrNoReclaim, hash)
	}

	signed, err := transactionFrom(signedXDR)
	if err != nil {
		return nil, err
	}
	returnedHash, err := signed.HashHex(s.settle.Network())
	if err != nil {
		return nil, fmt.Errorf("api: hash returned envelope: %w", err)
	}
	// The hash covers the transaction and not its signatures, so this is what
	// proves the envelope is byte-for-byte the one that was shown — and that
	// the balance about to be swept is still going where the member was told.
	if returnedHash != hash {
		return nil, fmt.Errorf("%w: signed %s, expected %s", ErrWrongEnvelope, returnedHash, hash)
	}

	claimed, err := s.store.ClaimReclaim(ctx, scope.Org, hash)
	if err != nil {
		return nil, err
	}

	result, err := s.settle.Submit(ctx, signed)
	if err != nil {
		// Left claimed. An ambiguous submission may still have landed, and
		// re-offering the same envelope would invite a second attempt at an
		// account that may no longer exist.
		return nil, err
	}

	if err := s.store.ReleaseAddress(ctx, scope.Org, claimed.Address); err != nil {
		return nil, err
	}
	return &ReclaimResult{Address: claimed.Address, Hash: result.Hash, Ledger: result.Ledger}, nil
}

// trustlineAsset turns a Horizon balance back into the asset a ChangeTrust
// names.
func trustlineAsset(b horizon.Balance) (txnbuild.ChangeTrustAsset, error) {
	if b.Asset.Code == "" || b.Asset.Issuer == "" {
		// A liquidity-pool share, or something this version does not model.
		// Refused rather than guessed at: a wrong asset here removes the wrong
		// trustline.
		return nil, fmt.Errorf(
			"%w: it holds a balance stelfin cannot name (%s)", ErrCannotReclaim, b.Asset.Type)
	}
	return txnbuild.CreditAsset{Code: b.Asset.Code, Issuer: b.Asset.Issuer}.MustToChangeTrustAsset(), nil
}
