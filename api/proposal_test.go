package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

// proposalFixture is a treasury fixture with a linked 2-of-2 treasury and a
// member to propose from.
type proposalFixture struct {
	*treasuryFixture
	treasuryRow store.Treasury
	member      store.MemberID
	co          *keypair.Full
}

func newProposalFixture(t *testing.T, name string) *proposalFixture {
	t.Helper()
	ctx := context.Background()

	f := newTreasuryFixture(t, name)
	co := keypair.MustRandom()
	f.twoOfTwo(co)

	tr, err := f.store.LinkTreasury(ctx, store.LinkTreasuryParams{
		Org: f.scope.Org, Kind: store.TreasuryClassic,
		Address: f.treasury.Address(), Label: "main",
		Low: 2, Medium: 2, High: 2,
	})
	if err != nil {
		t.Fatalf("link treasury: %v", err)
	}

	member, ok, err := f.store.MemberByIdentity(ctx, f.scope.Org, chat.Telegram, f.user)
	if err != nil || !ok {
		t.Fatalf("member: %v (found %v)", err, ok)
	}

	return &proposalFixture{treasuryFixture: f, treasuryRow: tr, member: member.ID, co: co}
}

func (f *proposalFixture) propose(t *testing.T) *ProposalView {
	t.Helper()
	view, err := f.svc.ProposePayment(context.Background(), f.scope, ProposePaymentParams{
		Treasury:    f.treasuryRow.ID,
		Destination: keypair.MustRandom().Address(),
		Amount:      money.MustParse("250"),
		Memo:        "August grants",
		CreatedBy:   f.member,
	})
	if err != nil {
		t.Fatalf("ProposePayment: %v", err)
	}
	return view
}

// signEnvelope signs a proposal's stored envelope, the way an approver's wallet
// would: the same envelope back, with one more signature on it.
func signEnvelope(t *testing.T, xdr string, kps ...*keypair.Full) string {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("not a simple transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, kps...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestProposePaymentOpensOneForTheTreasury(t *testing.T) {
	f := newProposalFixture(t, "propose")
	view := f.propose(t)

	if view.Proposal.Status != store.ProposalOpen {
		t.Fatalf("status = %q", view.Proposal.Status)
	}
	if view.Need != 2 || view.Have != 0 {
		t.Errorf("Have/Need = %d/%d, want 0/2", view.Have, view.Need)
	}
	if len(view.Missing) != 2 {
		t.Errorf("Missing = %v, want both signers", view.Missing)
	}
	// The envelope reserves the account's next sequence, which is the number
	// the guard at execution compares against.
	if view.Proposal.SourceSeq != 2 {
		t.Errorf("SourceSeq = %d, want 2", view.Proposal.SourceSeq)
	}
	// The signing window is the org's, not settlement.DefaultTimeout: three
	// minutes is fatally short for five people in three time zones.
	if window := time.Until(view.Proposal.ExpiresAt); window < 71*time.Hour {
		t.Errorf("signing window = %s, want the org's 72 hours", window)
	}
	if view.Description == nil || len(view.Description.Operations) != 1 {
		t.Fatalf("description = %+v", view.Description)
	}
	if view.Ready() {
		t.Error("an unsigned proposal reported itself ready")
	}
}

// TestOneOpenProposalPerTreasuryAtTheService: two open envelopes from one
// account reserve the same sequence number, so whichever executes first
// silently kills the other.
func TestOneOpenProposalPerTreasuryAtTheService(t *testing.T) {
	f := newProposalFixture(t, "propose twice")
	f.propose(t)

	_, err := f.svc.ProposePayment(context.Background(), f.scope, ProposePaymentParams{
		Treasury:    f.treasuryRow.ID,
		Destination: keypair.MustRandom().Address(),
		Amount:      money.MustParse("1"),
		CreatedBy:   f.member,
	})
	if !errors.Is(err, store.ErrProposalAlreadyOpen) {
		t.Fatalf("second proposal: %v", err)
	}
}

func TestApprovalsAccumulateToTheThreshold(t *testing.T) {
	f := newProposalFixture(t, "approve")
	ctx := context.Background()
	view := f.propose(t)

	after, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury), f.identity)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if after.Have != 1 || after.Ready() {
		t.Fatalf("one of two: Have = %d, ready = %v", after.Have, after.Ready())
	}
	if len(after.Signed) != 1 || after.Signed[0] != f.treasury.Address() {
		t.Errorf("Signed = %v", after.Signed)
	}

	after, err = f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.co), f.identity)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if after.Have != 2 || !after.Ready() {
		t.Fatalf("two of two: Have = %d, ready = %v", after.Have, after.Ready())
	}
	if len(after.Missing) != 0 {
		t.Errorf("Missing = %v after everyone signed", after.Missing)
	}
}

// TestApprovingTheSameEnvelopeTwiceIsHarmless: a member tapping approve twice
// on a slow connection has done nothing wrong, and counting it twice would
// announce a proposal ready that the network will refuse.
func TestApprovingTheSameEnvelopeTwiceIsHarmless(t *testing.T) {
	f := newProposalFixture(t, "approve twice")
	ctx := context.Background()
	view := f.propose(t)

	signed := signEnvelope(t, view.Proposal.XDR, f.treasury)
	for range 2 {
		if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID, signed, f.identity); err != nil {
			t.Fatalf("Approve: %v", err)
		}
	}
	after, err := f.svc.LoadProposal(ctx, f.scope, view.Proposal.ID)
	if err != nil {
		t.Fatalf("LoadProposal: %v", err)
	}
	if after.Have != 1 {
		t.Fatalf("Have = %d after the same signature twice, want 1", after.Have)
	}
}

// TestApprovalMustBeOverThisEnvelope is the mistake worth catching precisely:
// everything downstream treats a stored signature as approval of this payment.
func TestApprovalMustBeOverThisEnvelope(t *testing.T) {
	f := newProposalFixture(t, "wrong envelope")
	ctx := context.Background()
	view := f.propose(t)

	// A perfectly valid signature by a real signer, over a different
	// transaction from the same account.
	other, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: f.treasury.Address(), Sequence: 99,
		},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: keypair.MustRandom().Address(),
			Amount:      "9999",
			Asset:       txnbuild.NativeAsset{},
		}},
	})
	if err != nil {
		t.Fatalf("build other: %v", err)
	}
	signed, err := other.Sign(network.TestNetworkPassphrase, f.treasury, f.co)
	if err != nil {
		t.Fatalf("sign other: %v", err)
	}
	encoded, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode other: %v", err)
	}

	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID, encoded, f.identity); !errors.Is(
		err, ErrWrongEnvelope,
	) {
		t.Fatalf("a signature over another transaction was accepted: %v", err)
	}

	after, err := f.svc.LoadProposal(ctx, f.scope, view.Proposal.ID)
	if err != nil {
		t.Fatalf("LoadProposal: %v", err)
	}
	if after.Have != 0 {
		t.Fatalf("Have = %d after a rejected approval", after.Have)
	}
}

// TestAStrangersSignatureIsWorthNothing: well formed, verifies against its own
// key, and not on the account — which is the whole reason the account is asked
// rather than the envelope counted.
func TestAStrangersSignatureIsWorthNothing(t *testing.T) {
	f := newProposalFixture(t, "stranger")
	ctx := context.Background()
	view := f.propose(t)

	stranger := keypair.MustRandom()
	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, stranger), f.identity); !errors.Is(err, ErrWrongEnvelope) {
		t.Fatalf("a stranger's signature was accepted: %v", err)
	}
}

// TestARemovedSignerStopsCounting: the signer set is read from the network at
// every decision, so weight collected before a removal does not survive it.
func TestARemovedSignerStopsCounting(t *testing.T) {
	f := newProposalFixture(t, "removed")
	ctx := context.Background()
	view := f.propose(t)

	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury, f.co), f.identity); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The co-signer is removed on chain. Their stored signature is still a
	// valid signature and is no longer worth anything.
	f.twoOfTwoWithout(f.co)

	after, err := f.svc.LoadProposal(ctx, f.scope, view.Proposal.ID)
	if err != nil {
		t.Fatalf("LoadProposal: %v", err)
	}
	if after.Have != 1 || after.Ready() {
		t.Fatalf("Have = %d, ready = %v; a removed signer still counted", after.Have, after.Ready())
	}
}

func TestExecuteRefusesBeforeTheThreshold(t *testing.T) {
	f := newProposalFixture(t, "execute early")
	ctx := context.Background()
	view := f.propose(t)

	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury), f.identity); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := f.svc.Execute(ctx, f.scope, view.Proposal.ID, f.identity); !errors.Is(
		err, ErrNotEnoughSignatures,
	) {
		t.Fatalf("executed at one of two: %v", err)
	}
}

// TestExecuteCatchesAMovedSequence is the silent killer of multisig, made loud.
// The envelope is valid only at the sequence it reserved, so anything else the
// treasury submits meanwhile invalidates it permanently — and submitting anyway
// returns tx_bad_seq, which reads as a bug rather than as "somebody else spent
// from this account on Tuesday".
func TestExecuteCatchesAMovedSequence(t *testing.T) {
	f := newProposalFixture(t, "moved sequence")
	ctx := context.Background()
	view := f.propose(t)

	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury, f.co), f.identity); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Somebody else spent from the treasury in the meantime.
	f.horizon.fakeHorizon.sequence = 7

	_, err := f.svc.Execute(ctx, f.scope, view.Proposal.ID, f.identity)
	if !errors.Is(err, ErrSequenceMoved) {
		t.Fatalf("error = %v, want ErrSequenceMoved", err)
	}

	// And the proposal is closed rather than left to be retried forever against
	// a sequence that will never come back.
	after, err := f.store.Proposal(ctx, f.scope.Org, view.Proposal.ID)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if after.Status != store.ProposalFailed {
		t.Fatalf("status = %q, want failed", after.Status)
	}
	history, err := f.store.ProposalHistory(ctx, f.scope.Org, view.Proposal.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	last := history[len(history)-1]
	if last.Kind != store.ProposalFailed || last.Detail == "" {
		t.Fatalf("the audit trail does not say why: %+v", last)
	}
}

func TestExecuteSubmitsAndResolves(t *testing.T) {
	f := newProposalFixture(t, "execute")
	ctx := context.Background()
	view := f.propose(t)

	if _, err := f.svc.Approve(ctx, f.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury, f.co), f.identity); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	result, err := f.svc.Execute(ctx, f.scope, view.Proposal.ID, f.identity)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Hash == "" {
		t.Fatal("no transaction hash")
	}

	after, err := f.store.Proposal(ctx, f.scope.Org, view.Proposal.ID)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if after.Status != store.ProposalExecuted || after.SubmittedTx != result.Hash {
		t.Fatalf("resolved as %+v", after)
	}

	// Executed once. A second attempt is not a second payment.
	if _, err := f.svc.Execute(ctx, f.scope, view.Proposal.ID, f.identity); !errors.Is(
		err, store.ErrProposalClosed,
	) {
		t.Fatalf("second execution: %v", err)
	}
}

func TestProposalReadsAreScopedToTheirOrgAtTheService(t *testing.T) {
	f := newProposalFixture(t, "scope a")
	other := newProposalFixture(t, "scope b")
	ctx := context.Background()
	view := f.propose(t)

	if _, err := f.svc.LoadProposal(ctx, other.scope, view.Proposal.ID); !errors.Is(
		err, store.ErrNoProposal,
	) {
		t.Errorf("LoadProposal across orgs: %v", err)
	}
	if _, err := f.svc.Approve(ctx, other.scope, view.Proposal.ID,
		signEnvelope(t, view.Proposal.XDR, f.treasury), other.identity); !errors.Is(
		err, store.ErrNoProposal,
	) {
		t.Errorf("Approve across orgs: %v", err)
	}
	if _, err := f.svc.Execute(ctx, other.scope, view.Proposal.ID, other.identity); !errors.Is(
		err, store.ErrNoProposal,
	) {
		t.Errorf("Execute across orgs: %v", err)
	}
}
