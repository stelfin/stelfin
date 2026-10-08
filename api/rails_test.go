package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
)

func (f *proposalFixture) proposeTo(to string, amount string) (*ProposalView, error) {
	return f.svc.ProposePayment(context.Background(), f.scope, ProposePaymentParams{
		Treasury: f.treasuryRow.ID, Destination: to,
		Amount: money.MustParse(amount), CreatedBy: f.member,
	})
}

// openProposals counts what is waiting on signers for a fixture's treasury.
func (f *proposalFixture) openProposals(t *testing.T) int {
	t.Helper()
	_, ok, err := f.store.OpenProposal(context.Background(), f.scope.Org, f.treasuryRow.ID)
	if err != nil {
		t.Fatalf("open proposal: %v", err)
	}
	if ok {
		return 1
	}
	return 0
}

// TestAPaymentToNowhereNeverReachesASigner: on Stellar a payment to an account
// that does not exist, or that has no trustline, fails — at submission, after
// every signature has been collected. Signatures are the one resource a DAO
// has, so the refusal has to come before anybody is asked for one.
func TestAPaymentToNowhereNeverReachesASigner(t *testing.T) {
	f := newProposalFixture(t, "preflight")
	missing := keypair.MustRandom().Address()
	bare := keypair.MustRandom().Address()
	f.horizon.absent = map[string]bool{missing: true}
	f.horizon.bare = map[string]bool{bare: true}

	for name, to := range map[string]string{"an account that does not exist": missing, "an account with no trustline": bare} {
		_, err := f.proposeTo(to, "250")
		var notReady *DestinationError
		if !errors.As(err, &notReady) || !errors.Is(err, ErrDestinationNotReady) {
			t.Fatalf("%s: error = %v, want a DestinationError", name, err)
		}
		if notReady.Address != to || notReady.Reason == "" {
			t.Errorf("%s: error does not say what is wrong with it: %+v", name, notReady)
		}
	}
	if n := f.openProposals(t); n != 0 {
		t.Fatalf("%d proposals were opened for payments that cannot land", n)
	}

	// And one that can be paid still is.
	if _, err := f.proposeTo(keypair.MustRandom().Address(), "250"); err != nil {
		t.Fatalf("a payable destination was refused: %v", err)
	}
}

// TestAWorkspaceCeilingStopsAProposalBeforeItIsBuilt: a classic multisig has no
// contract to enforce anything, so these are the only limits there are. The
// failure they prevent is not theft — it is four tired people approving an
// extra zero.
func TestAWorkspaceCeilingStopsAProposalBeforeItIsBuilt(t *testing.T) {
	f := newProposalFixture(t, "ceiling")
	ctx := context.Background()
	if err := f.store.SetSpendPolicy(ctx, f.scope.Org, store.SpendPolicy{
		PerPayment: money.MustParse("500"), Daily: money.MustParse("700"),
	}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	to := keypair.MustRandom().Address()

	_, err := f.proposeTo(to, "500.01")
	var over *CeilingError
	if !errors.As(err, &over) || over.Scope != "workspace" || over.Window != "payment" {
		t.Fatalf("error = %v, want the workspace's per-payment ceiling", err)
	}
	if n := f.openProposals(t); n != 0 {
		t.Fatal("a proposal over the ceiling was opened")
	}

	// Exactly at the ceiling is allowed: a ceiling is the largest thing
	// permitted, not the smallest thing refused.
	view, err := f.proposeTo(to, "500")
	if err != nil {
		t.Fatalf("a payment at the ceiling was refused: %v", err)
	}

	// The open proposal counts toward the day. Otherwise a day's ceiling could
	// be queued up several times over and then signed at leisure.
	// Executed, so the treasury is free for another — and still counted.
	if err := f.store.ResolveProposal(ctx, f.scope.Org, view.Proposal.ID,
		store.ProposalExecuted, view.Proposal.Hash, "", 0); err != nil {
		t.Fatalf("close the first proposal: %v", err)
	}
	_, err = f.proposeTo(to, "200.01")
	if !errors.As(err, &over) || over.Window != "day" || over.Asked != money.MustParse("700.01") {
		t.Fatalf("error = %v, want the daily ceiling at 700.01", err)
	}
	if _, err := f.proposeTo(to, "200"); err != nil {
		t.Fatalf("a payment that exactly fills the day was refused: %v", err)
	}
}

// The deployment's ceilings sit above every workspace's, and a workspace with
// none of its own is still under them.
func TestTheDeploymentCeilingAppliesToAWorkspaceWithNone(t *testing.T) {
	f := newProposalFixture(t, "deployment ceiling")
	f.svc.cfg.MaxPayment = money.MustParse("1000")

	_, err := f.proposeTo(keypair.MustRandom().Address(), "1000.0000001")
	var over *CeilingError
	if !errors.As(err, &over) || over.Scope != "deployment" {
		t.Fatalf("error = %v, want the deployment's ceiling", err)
	}
	if _, err := f.proposeTo(keypair.MustRandom().Address(), "1000"); err != nil {
		t.Fatalf("a payment at the deployment ceiling was refused: %v", err)
	}
}

// TestReadOnlyBuildsAndSubmitsNothing is the deployment-wide kill switch.
// Reading still works — an incident is exactly when people need to look.
func TestReadOnlyBuildsAndSubmitsNothing(t *testing.T) {
	f := newProposalFixture(t, "read only")
	ctx := context.Background()
	view := f.propose(t)

	f.svc.cfg.ReadOnly = true

	if _, err := f.proposeTo(keypair.MustRandom().Address(), "1"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("ProposePayment: %v, want ErrReadOnly", err)
	}
	if _, err := f.svc.ExecuteProposal(ctx, f.scope, view.Proposal.ID, 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("ExecuteProposal: %v, want ErrReadOnly", err)
	}
	if _, err := f.svc.PrepareSend(ctx, f.scope, []string{"send 5 to ada"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("PrepareSend: %v, want ErrReadOnly", err)
	}
	if _, err := f.svc.Submit(ctx, f.scope, "AAAA", f.treasury.Address(), signWith(f.treasury)); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Submit: %v, want ErrReadOnly", err)
	}
	if _, err := f.svc.PrepareEnrollment(ctx, f.scope, keypair.MustRandom().Address(), f.treasury.Address()); !errors.Is(err, ErrReadOnly) {
		t.Errorf("PrepareEnrollment: %v, want ErrReadOnly", err)
	}
	if len(f.horizon.submitted) != 0 {
		t.Fatalf("%d transactions reached the network in read-only mode", len(f.horizon.submitted))
	}

	if _, err := f.svc.LoadProposal(ctx, f.scope, view.Proposal.ID); err != nil {
		t.Errorf("a read was refused in read-only mode: %v", err)
	}
}

// The file switch takes effect on the next write, with no restart — and comes
// off the same way.
func TestTheReadOnlyFileStopsWritesWhileItExists(t *testing.T) {
	f := newProposalFixture(t, "read only file")
	flag := filepath.Join(t.TempDir(), "readonly")
	f.svc.cfg.ReadOnlyFile = flag
	to := keypair.MustRandom().Address()

	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatalf("create flag: %v", err)
	}
	if _, err := f.proposeTo(to, "1"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("with the file present: %v, want ErrReadOnly", err)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatalf("remove flag: %v", err)
	}
	if _, err := f.proposeTo(to, "1"); err != nil {
		t.Fatalf("with the file gone: %v", err)
	}
}
