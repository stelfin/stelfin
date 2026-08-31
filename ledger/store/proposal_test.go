package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
)

var proposalSeq int

// newProposal opens a proposal against tn's treasury.
func newProposal(t *testing.T, s *Store, tn *tenant, tr Treasury, ttl time.Duration) Proposal {
	t.Helper()
	proposalSeq++
	text := fmt.Sprintf("pay\t100.0000000\tUSDC\t%d", proposalSeq)
	digest := sha256.Sum256([]byte(text))

	p, err := s.CreateProposal(context.Background(), CreateProposalParams{
		Org:         tn.org.ID,
		Treasury:    tr.ID,
		Kind:        "payment",
		XDR:         fmt.Sprintf("AAAAAgAAenvelope%d", proposalSeq),
		Hash:        fmt.Sprintf("%064x", proposalSeq),
		SourceSeq:   int64(4_000_000 + proposalSeq),
		Description: text,
		Digest:      digest[:],
		CreatedBy:   tn.member.ID,
		ExpiresAt:   time.Now().Add(ttl),
	})
	must(t, err, "create proposal")
	return p
}

func signature(n byte) []byte {
	sig := make([]byte, 64)
	for i := range sig {
		sig[i] = n
	}
	return sig
}

func TestProposalRoundTrip(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-round")
	tr := linkTreasury(t, s, tn, 2)

	p := newProposal(t, s, tn, tr, time.Hour)
	if p.Status != ProposalOpen || p.Treasury != tr.ID {
		t.Fatalf("created %+v", p)
	}

	got, err := s.Proposal(ctx, tn.org.ID, p.ID)
	must(t, err, "read proposal")
	if got.XDR != p.XDR || got.SourceSeq != p.SourceSeq || !bytes.Equal(got.Digest, p.Digest) {
		t.Fatalf("read back %+v, want %+v", got, p)
	}

	open, ok, err := s.OpenProposal(ctx, tn.org.ID, tr.ID)
	must(t, err, "open proposal")
	if !ok || open.ID != p.ID {
		t.Fatalf("OpenProposal = %+v, %v", open, ok)
	}

	// Creation is on the record from the first line, not from whoever signed
	// first.
	history, err := s.ProposalHistory(ctx, tn.org.ID, p.ID)
	must(t, err, "history")
	if len(history) != 1 || history[0].Kind != EventCreated {
		t.Fatalf("history = %+v", history)
	}
}

// TestOneOpenProposalPerTreasury: two open proposals from one account both
// reserve the same next sequence number, so whichever executes first silently
// kills the other. The refusal is what turns that into something a member can
// see.
func TestOneOpenProposalPerTreasury(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-one-open")
	tr := linkTreasury(t, s, tn, 2)

	first := newProposal(t, s, tn, tr, time.Hour)

	proposalSeq++
	digest := sha256.Sum256([]byte("second"))
	_, err := s.CreateProposal(ctx, CreateProposalParams{
		Org: tn.org.ID, Treasury: tr.ID, Kind: "payment",
		XDR: "AAAAAgAAsecond", Hash: fmt.Sprintf("%064x", proposalSeq),
		SourceSeq: 9, Description: "second", Digest: digest[:],
		CreatedBy: tn.member.ID, ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrProposalAlreadyOpen) {
		t.Fatalf("second open proposal: %v", err)
	}

	// Once the first is resolved the slot is free again.
	must(t, s.ResolveProposal(ctx, tn.org.ID, first.ID, ProposalCancelled, "", "changed our minds", 0),
		"cancel first")
	newProposal(t, s, tn, tr, time.Hour)
}

// TestSignatureIsIdempotentPerSigner: the same key signing twice is not more
// approval. A member tapping approve twice on a slow connection has done
// nothing wrong, so the second write changes nothing rather than erroring.
func TestSignatureIsIdempotentPerSigner(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-idem")
	tr := linkTreasury(t, s, tn, 2)
	p := newProposal(t, s, tn, tr, time.Hour)

	ada := keypair.MustRandom().Address()
	sig := ProposalSignature{Signer: ada, Signature: signature(1), Weight: 1, AddedBy: identityOf(t, tn)}

	must(t, s.AddSignature(ctx, tn.org.ID, p.ID, sig), "sign")
	must(t, s.AddSignature(ctx, tn.org.ID, p.ID, sig), "sign again")

	sigs, err := s.Signatures(ctx, tn.org.ID, p.ID)
	must(t, err, "signatures")
	if len(sigs) != 1 {
		t.Fatalf("%d signature(s) for one signer", len(sigs))
	}
	if !bytes.Equal(sigs[0].Signature, sig.Signature) || sigs[0].Signer != ada {
		t.Fatalf("stored %+v", sigs[0])
	}

	// And the audit trail does not claim two people approved.
	history, err := s.ProposalHistory(ctx, tn.org.ID, p.ID)
	must(t, err, "history")
	signed := 0
	for _, e := range history {
		if e.Kind == EventSigned {
			signed++
		}
	}
	if signed != 1 {
		t.Fatalf("%d 'signed' events for one signer", signed)
	}
}

func TestSignaturesAccumulate(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-accum")
	tr := linkTreasury(t, s, tn, 2)
	p := newProposal(t, s, tn, tr, time.Hour)

	for i := byte(1); i <= 3; i++ {
		must(t, s.AddSignature(ctx, tn.org.ID, p.ID, ProposalSignature{
			Signer: keypair.MustRandom().Address(), Signature: signature(i), Weight: 1,
		}), "sign")
	}
	sigs, err := s.Signatures(ctx, tn.org.ID, p.ID)
	must(t, err, "signatures")
	if len(sigs) != 3 {
		t.Fatalf("%d signature(s), want 3", len(sigs))
	}
}

// TestApprovalAfterTheFactIsRefused: an approval landing on an envelope that
// has already been submitted is an approval nobody can withdraw.
func TestApprovalAfterTheFactIsRefused(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-late")
	tr := linkTreasury(t, s, tn, 1)
	p := newProposal(t, s, tn, tr, time.Hour)

	must(t, s.ResolveProposal(ctx, tn.org.ID, p.ID, ProposalExecuted,
		fmt.Sprintf("%064x", 0xdeadbeef), "", identityOf(t, tn)), "execute")

	err := s.AddSignature(ctx, tn.org.ID, p.ID, ProposalSignature{
		Signer: keypair.MustRandom().Address(), Signature: signature(9), Weight: 1,
	})
	if !errors.Is(err, ErrProposalClosed) {
		t.Fatalf("signing a closed proposal: %v", err)
	}
}

func TestApprovalAfterExpiryIsRefused(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-expired")
	tr := linkTreasury(t, s, tn, 1)
	p := newProposal(t, s, tn, tr, 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)

	err := s.AddSignature(ctx, tn.org.ID, p.ID, ProposalSignature{
		Signer: keypair.MustRandom().Address(), Signature: signature(8), Weight: 1,
	})
	if !errors.Is(err, ErrProposalClosed) {
		t.Fatalf("signing an expired proposal: %v", err)
	}
}

// TestResolveIsCompareAndSet: two members racing to execute must produce one
// resolution and one plain "no longer open", not a second write that overwrites
// the first outcome.
func TestResolveIsCompareAndSet(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-cas")
	tr := linkTreasury(t, s, tn, 1)
	p := newProposal(t, s, tn, tr, time.Hour)

	hash := fmt.Sprintf("%064x", 0xabc)
	must(t, s.ResolveProposal(ctx, tn.org.ID, p.ID, ProposalExecuted, hash, "", 0), "execute")

	err := s.ResolveProposal(ctx, tn.org.ID, p.ID, ProposalFailed, "", "second try", 0)
	if !errors.Is(err, ErrProposalClosed) {
		t.Fatalf("second resolution: %v", err)
	}

	got, err := s.Proposal(ctx, tn.org.ID, p.ID)
	must(t, err, "read proposal")
	if got.Status != ProposalExecuted || got.SubmittedTx != hash {
		t.Fatalf("outcome was overwritten: %+v", got)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at was not set")
	}
}

// TestProposalEnvelopeCannotBeRewritten goes around the store on purpose. The
// guarantee has to survive a future method, a migration, or a psql session —
// approvals attached to a document nobody approved is the one way a correctly
// implemented multisig still loses money.
func TestProposalEnvelopeCannotBeRewritten(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-immutable")
	tr := linkTreasury(t, s, tn, 1)
	p := newProposal(t, s, tn, tr, time.Hour)

	for _, stmt := range []string{
		`UPDATE proposals SET base_xdr = 'AAAAsomethingelse' WHERE id = $1`,
		`UPDATE proposals SET source_seq = source_seq + 1 WHERE id = $1`,
		`UPDATE proposals SET description_canonical = 'pay 1 XLM' WHERE id = $1`,
	} {
		if _, err := testPool.Exec(ctx, stmt, int64(p.ID)); err == nil {
			t.Errorf("allowed: %s", stmt)
		}
	}

	// A status change is still permitted; it is the document that is frozen,
	// not the row.
	must(t, s.ResolveProposal(ctx, tn.org.ID, p.ID, ProposalCancelled, "", "", 0), "cancel")
}

func TestExpireProposalsFreesTheSlot(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	tn := newTenant(t, s, "pr-sweep")
	tr := linkTreasury(t, s, tn, 1)
	p := newProposal(t, s, tn, tr, 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)

	if _, err := s.ExpireProposals(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}

	got, err := s.Proposal(ctx, tn.org.ID, p.ID)
	must(t, err, "read proposal")
	if got.Status != ProposalExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}

	// The treasury's one slot is free again without anyone having looked at the
	// dead proposal.
	newProposal(t, s, tn, tr, time.Hour)

	history, err := s.ProposalHistory(ctx, tn.org.ID, p.ID)
	must(t, err, "history")
	if len(history) != 2 || history[1].Kind != EventExpired {
		t.Fatalf("history = %+v", history)
	}
}

func TestProposalReadsAreScopedToTheirOrg(t *testing.T) {
	s := New(testPool)
	ctx := context.Background()
	a := newTenant(t, s, "pr-scope-a")
	b := newTenant(t, s, "pr-scope-b")

	tr := linkTreasury(t, s, a, 1)
	p := newProposal(t, s, a, tr, time.Hour)
	must(t, s.AddSignature(ctx, a.org.ID, p.ID, ProposalSignature{
		Signer: keypair.MustRandom().Address(), Signature: signature(2), Weight: 1,
	}), "sign")

	if _, err := s.Proposal(ctx, b.org.ID, p.ID); !errors.Is(err, ErrNoProposal) {
		t.Errorf("Proposal across orgs: %v", err)
	}
	if _, ok, err := s.OpenProposal(ctx, b.org.ID, tr.ID); err != nil || ok {
		t.Errorf("OpenProposal across orgs: %v, %v", ok, err)
	}
	if got, err := s.Proposals(ctx, b.org.ID, 10); err != nil || len(got) != 0 {
		t.Errorf("Proposals across orgs: %d row(s), %v", len(got), err)
	}
	if got, err := s.Signatures(ctx, b.org.ID, p.ID); err != nil || len(got) != 0 {
		t.Errorf("Signatures across orgs: %d row(s), %v", len(got), err)
	}
	if got, err := s.ProposalHistory(ctx, b.org.ID, p.ID); err != nil || len(got) != 0 {
		t.Errorf("ProposalHistory across orgs: %d row(s), %v", len(got), err)
	}
	if err := s.AddSignature(ctx, b.org.ID, p.ID, ProposalSignature{
		Signer: keypair.MustRandom().Address(), Signature: signature(3), Weight: 1,
	}); !errors.Is(err, ErrNoProposal) {
		t.Errorf("AddSignature across orgs: %v", err)
	}
	if err := s.ResolveProposal(ctx, b.org.ID, p.ID, ProposalCancelled, "", "", 0); !errors.Is(
		err, ErrProposalClosed,
	) {
		t.Errorf("ResolveProposal across orgs: %v", err)
	}

	// And after all that it is still open, still A's, still with one signature.
	still, err := s.Proposal(ctx, a.org.ID, p.ID)
	must(t, err, "read proposal")
	if still.Status != ProposalOpen {
		t.Fatalf("status = %q", still.Status)
	}
	sigs, err := s.Signatures(ctx, a.org.ID, p.ID)
	must(t, err, "signatures")
	if len(sigs) != 1 {
		t.Fatalf("%d signature(s)", len(sigs))
	}
}
