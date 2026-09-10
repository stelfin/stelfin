package connector_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stelfin/stelfin/connector"
)

// grants is a stand-in for the on-chain registry.
type grants struct {
	grant connector.Grant
	err   error
	calls int
}

func (g *grants) GrantFor(context.Context, int64, string) (connector.Grant, error) {
	g.calls++
	return g.grant, g.err
}

// sheet is a Reader that records what it was asked for.
type sheet struct {
	descriptor connector.Descriptor
	requests   []connector.Request
	rows       int
}

func (s *sheet) Describe() connector.Descriptor { return s.descriptor }

func (s *sheet) Read(_ context.Context, r connector.Request) (connector.Observation, error) {
	s.requests = append(s.requests, r)
	rows := make([][]connector.Untrusted[string], 0, s.rows)
	for i := 0; i < s.rows; i++ {
		rows = append(rows, []connector.Untrusted[string]{connector.Wrap("cell")})
	}
	return connector.Observation{Rows: rows, ReadAt: time.Unix(1700000000, 0)}, nil
}

// payroll is a Proposer.
type payroll struct{ descriptor connector.Descriptor }

func (p payroll) Describe() connector.Descriptor { return p.descriptor }

func (payroll) Propose(context.Context, connector.Request) (connector.Draft, error) {
	return connector.Draft{Rows: []connector.DraftRow{{
		Line:            1,
		AmountText:      connector.Wrap("250"),
		DestinationText: connector.Wrap("ada"),
	}}}, nil
}

func sheetDescriptor() connector.Descriptor {
	return connector.Descriptor{
		ID:           "sheets",
		Kind:         connector.KindReader,
		Capabilities: []string{"read.range", "read.metadata"},
	}
}

func fullGrant() connector.Grant {
	return connector.Grant{
		Capabilities:  []string{"read.range", "read.metadata"},
		SurfaceDigest: connector.SurfaceDigest(sheetDescriptor()),
		PerHour:       10,
	}
}

func newGuard(t *testing.T, g *grants, now connector.Clock) *connector.Guard {
	t.Helper()
	guard, err := connector.NewGuard(g, now)
	if err != nil {
		t.Fatalf("new guard: %v", err)
	}
	return guard
}

func TestAGrantedReadGoesThrough(t *testing.T) {
	g := &grants{grant: fullGrant()}
	guard := newGuard(t, g, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 3}

	got, err := guard.Read(context.Background(), 42, s, "read.range",
		connector.Request{Selector: "Payroll!A1:C50", Limit: 50})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Rows) != 3 {
		t.Fatalf("%d rows", len(got.Rows))
	}
	if len(s.requests) != 1 || s.requests[0].Selector != "Payroll!A1:C50" {
		t.Fatalf("the connector was asked for %+v", s.requests)
	}
}

// TestAChangedSurfaceIsRefused is the pin a capability list cannot provide.
//
// A connector granted "read a range" that quietly starts offering a tool which
// transfers funds passes every capability check, because the capability name it
// is called with did not change. The digest is what turns that into a refusal.
func TestAChangedSurfaceIsRefused(t *testing.T) {
	g := &grants{grant: fullGrant()}
	guard := newGuard(t, g, nil)

	grown := sheetDescriptor()
	grown.Capabilities = append(grown.Capabilities, "write.transfer")
	s := &sheet{descriptor: grown, rows: 1}

	_, err := guard.Read(context.Background(), 42, s, "read.range", connector.Request{})
	if !errors.Is(err, connector.ErrSurfaceChanged) {
		t.Fatalf("error = %v, want ErrSurfaceChanged", err)
	}
	// It never reached the connector.
	if len(s.requests) != 0 {
		t.Error("the connector was called anyway")
	}
}

func TestAnUngrantedCapabilityIsRefused(t *testing.T) {
	g := &grants{grant: fullGrant()}
	guard := newGuard(t, g, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}

	_, err := guard.Read(context.Background(), 42, s, "read.everything", connector.Request{})
	if !errors.Is(err, connector.ErrCapabilityNotGranted) {
		t.Fatalf("error = %v, want ErrCapabilityNotGranted", err)
	}
	// An empty capability is not a wildcard.
	if _, err := guard.Read(context.Background(), 42, s, "", connector.Request{}); !errors.Is(
		err, connector.ErrCapabilityNotGranted,
	) {
		t.Fatalf("an empty capability: %v", err)
	}
	if len(s.requests) != 0 {
		t.Error("the connector was called anyway")
	}
}

// TestAConnectorWithNoAllowanceDoesNotRun: zero is what a grant carries before
// anybody sets a rate, and it has to fail closed.
func TestAConnectorWithNoAllowanceDoesNotRun(t *testing.T) {
	grant := fullGrant()
	grant.PerHour = 0
	guard := newGuard(t, &grants{grant: grant}, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}

	_, err := guard.Read(context.Background(), 42, s, "read.range", connector.Request{})
	if !errors.Is(err, connector.ErrRateExceeded) {
		t.Fatalf("error = %v, want ErrRateExceeded", err)
	}
}

func TestTheRateIsCountedPerOrgAndPerConnector(t *testing.T) {
	now := time.Unix(1700000000, 0)
	grant := fullGrant()
	grant.PerHour = 2
	guard := newGuard(t, &grants{grant: grant}, func() time.Time { return now })
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := guard.Read(ctx, 42, s, "read.range", connector.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := guard.Read(ctx, 42, s, "read.range", connector.Request{}); !errors.Is(
		err, connector.ErrRateExceeded,
	) {
		t.Fatalf("the third call: %v", err)
	}

	// Another org has its own allowance: one tenant must not be able to
	// exhaust another's.
	if _, err := guard.Read(ctx, 43, s, "read.range", connector.Request{}); err != nil {
		t.Fatalf("another org was rate-limited by the first: %v", err)
	}

	// And the window rolls forward from the first call.
	now = now.Add(time.Hour + time.Second)
	if _, err := guard.Read(ctx, 42, s, "read.range", connector.Request{}); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// TestARefusedCallCostsNoAllowance: otherwise refusing a call becomes a way to
// exhaust a connector's rate, which costs the attacker nothing.
func TestARefusedCallCostsNoAllowance(t *testing.T) {
	now := time.Unix(1700000000, 0)
	grant := fullGrant()
	grant.PerHour = 2
	guard := newGuard(t, &grants{grant: grant}, func() time.Time { return now })
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := guard.Read(ctx, 42, s, "read.nothing", connector.Request{}); !errors.Is(
			err, connector.ErrCapabilityNotGranted,
		) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// The allowance is untouched.
	for i := 0; i < 2; i++ {
		if _, err := guard.Read(ctx, 42, s, "read.range", connector.Request{}); err != nil {
			t.Fatalf("granted call %d was refused: %v", i, err)
		}
	}
}

// TestAConnectorCannotBeUsedAsTheOtherKind.
func TestAConnectorCannotBeUsedAsTheOtherKind(t *testing.T) {
	guard := newGuard(t, &grants{grant: fullGrant()}, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}
	ctx := context.Background()

	if _, err := guard.Propose(ctx, 42, s, "read.range", connector.Request{}); !errors.Is(
		err, connector.ErrNotAProposer,
	) {
		t.Fatalf("a reader proposed: %v", err)
	}

	p := payroll{descriptor: connector.Descriptor{
		ID: "payroll", Kind: connector.KindProposer, Capabilities: []string{"propose.batch"},
	}}
	if _, err := guard.Read(ctx, 42, p, "propose.batch", connector.Request{}); !errors.Is(
		err, connector.ErrNotAReader,
	) {
		t.Fatalf("a proposer was read: %v", err)
	}
}

func TestAGrantedProposalGoesThrough(t *testing.T) {
	descriptor := connector.Descriptor{
		ID: "payroll", Kind: connector.KindProposer, Capabilities: []string{"propose.batch"},
	}
	guard := newGuard(t, &grants{grant: connector.Grant{
		Capabilities:  []string{"propose.batch"},
		SurfaceDigest: connector.SurfaceDigest(descriptor),
		PerHour:       5,
	}}, nil)

	draft, err := guard.Propose(context.Background(), 42,
		payroll{descriptor: descriptor}, "propose.batch", connector.Request{})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if len(draft.Rows) != 1 || draft.Rows[0].AmountText.Unwrap() != "250" {
		t.Fatalf("draft = %+v", draft.Rows)
	}
}

// TestEveryCallIsBounded: a connector returning a million rows is not an attack
// that needs to succeed to hurt — the memory is spent before anybody decides
// the answer is unreasonable.
func TestEveryCallIsBounded(t *testing.T) {
	guard := newGuard(t, &grants{grant: fullGrant()}, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}
	ctx := context.Background()

	for _, asked := range []int{0, -1, connector.MaxRows + 1, 1 << 20} {
		s.requests = nil
		if _, err := guard.Read(ctx, 42, s, "read.range",
			connector.Request{Limit: asked}); err != nil {
			t.Fatalf("limit %d: %v", asked, err)
		}
		if got := s.requests[0].Limit; got != connector.MaxRows {
			t.Errorf("asked for %d, the connector was told %d", asked, got)
		}
	}

	// A reasonable limit is passed through untouched.
	s.requests = nil
	if _, err := guard.Read(ctx, 42, s, "read.range",
		connector.Request{Limit: 50}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.requests[0].Limit != 50 {
		t.Errorf("a limit of 50 became %d", s.requests[0].Limit)
	}
}

// TestTheSurfaceDigestDependsOnEverythingItPins.
func TestTheSurfaceDigestDependsOnEverythingItPins(t *testing.T) {
	base := sheetDescriptor()
	digest := connector.SurfaceDigest(base)

	// Order does not matter: reordering a list is not a different surface, and
	// two builds of one connector must agree.
	reordered := base
	reordered.Capabilities = []string{"read.metadata", "read.range"}
	if connector.SurfaceDigest(reordered) != digest {
		t.Error("reordering the capabilities changed the digest")
	}

	for name, changed := range map[string]connector.Descriptor{
		"a different id":      {ID: "other", Kind: base.Kind, Capabilities: base.Capabilities},
		"a different kind":    {ID: base.ID, Kind: connector.KindProposer, Capabilities: base.Capabilities},
		"one more capability": {ID: base.ID, Kind: base.Kind, Capabilities: append(append([]string(nil), base.Capabilities...), "write")},
		"one fewer":           {ID: base.ID, Kind: base.Kind, Capabilities: base.Capabilities[:1]},
	} {
		if connector.SurfaceDigest(changed) == digest {
			t.Errorf("%s did not change the digest", name)
		}
	}

	// Length-prefixed, so ["read","write"] and ["readwrite"] cannot collide —
	// the second would otherwise inherit the first's grant.
	split := connector.Descriptor{ID: "x", Kind: connector.KindReader,
		Capabilities: []string{"read", "write"}}
	joined := connector.Descriptor{ID: "x", Kind: connector.KindReader,
		Capabilities: []string{"readwrite"}}
	if connector.SurfaceDigest(split) == connector.SurfaceDigest(joined) {
		t.Error("two different capability lists hash identically")
	}
}

func TestAGrantWithNoPinnedSurfaceIsRefused(t *testing.T) {
	grant := fullGrant()
	grant.SurfaceDigest = ""
	guard := newGuard(t, &grants{grant: grant}, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}

	if _, err := guard.Read(context.Background(), 42, s, "read.range",
		connector.Request{}); !errors.Is(err, connector.ErrNotGranted) {
		t.Fatalf("error = %v, want ErrNotGranted", err)
	}
}

func TestAFailureToReadTheGrantStopsTheCall(t *testing.T) {
	guard := newGuard(t, &grants{err: errors.New("rpc unreachable")}, nil)
	s := &sheet{descriptor: sheetDescriptor(), rows: 1}

	if _, err := guard.Read(context.Background(), 42, s, "read.range",
		connector.Request{}); err == nil {
		t.Fatal("the call proceeded without knowing whether it was granted")
	}
	if len(s.requests) != 0 {
		t.Error("the connector was called anyway")
	}
}
