package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The one gate every connector call goes through.
//
// Not one check per call site. A guard that has to be remembered is a guard
// that is forgotten once, and the once is the whole exposure — so every path
// into a connector runs through this and there is no other exported way to
// invoke one.
//
// What it checks, in this order:
//
//  1. that the DAO granted this connector anything at all, on chain, where the
//     DAO can see it and stelfin cannot alter it;
//  2. that the tool surface the connector is presenting is the one the grant
//     was issued against;
//  3. that the capability being used is named in the grant;
//  4. that the call is inside the rate this deployment allows.
//
// The order matters at the edges. A connector with no grant is refused before
// its surface is even asked for, and the rate limit is spent last, so a call
// that was never going to be allowed does not consume an allowance on its way
// to being refused.

var (
	// ErrSurfaceChanged reports a connector offering something other than what
	// the DAO agreed to.
	ErrSurfaceChanged = errors.New("connector: this connector's tools have changed since it was granted")

	// ErrCapabilityNotGranted reports a call the grant does not cover.
	ErrCapabilityNotGranted = errors.New("connector: that capability was not granted")

	// ErrRateExceeded reports a connector called too often.
	ErrRateExceeded = errors.New("connector: this connector has been called too often")

	// ErrNotAReader and ErrNotAProposer report a connector used as the other
	// kind. Reachable only through a wiring mistake, and worth being an error
	// rather than a panic: the mistake belongs to a deployment, not a user.
	ErrNotAReader   = errors.New("connector: that connector does not read")
	ErrNotAProposer = errors.New("connector: that connector does not propose")
)

// Grants reports what a DAO has authorised.
//
// An interface, so this package neither reads the chain nor knows how. The
// authority is the on-chain registry; where that is read from is somebody
// else's problem, and keeping it that way is what lets the import-graph test
// hold.
type Grants interface {
	// GrantFor returns what an org has authorised for one connector.
	GrantFor(ctx context.Context, org int64, connector string) (Grant, error)
}

// Grant is a DAO's authorisation, in the terms this package checks.
type Grant struct {
	// Capabilities the connector may use.
	Capabilities []string
	// SurfaceDigest pins what the connector said it offers.
	SurfaceDigest string
	// PerHour bounds how often it may be called. Zero means never.
	PerHour int
}

// Clock is time, injectable so a rate limit can be tested without waiting.
type Clock func() time.Time

// Guard authorises connector calls.
type Guard struct {
	grants Grants
	now    Clock

	limiter *rateLimiter
}

// NewGuard returns a Guard.
func NewGuard(grants Grants, now Clock) (*Guard, error) {
	if grants == nil {
		return nil, errors.New("connector: a guard needs somewhere to read grants from")
	}
	if now == nil {
		now = time.Now
	}
	return &Guard{grants: grants, now: now, limiter: newRateLimiter()}, nil
}

// Read runs a Reader behind the guard.
func (g *Guard) Read(
	ctx context.Context, org int64, c any, capability string, request Request,
) (Observation, error) {
	reader, ok := c.(Reader)
	if !ok {
		return Observation{}, fmt.Errorf("%w: %T", ErrNotAReader, c)
	}
	if err := g.authorise(ctx, org, reader.Describe(), capability); err != nil {
		return Observation{}, err
	}
	return reader.Read(ctx, bound(request))
}

// Propose runs a Proposer behind the guard.
func (g *Guard) Propose(
	ctx context.Context, org int64, c any, capability string, request Request,
) (Draft, error) {
	proposer, ok := c.(Proposer)
	if !ok {
		return Draft{}, fmt.Errorf("%w: %T", ErrNotAProposer, c)
	}
	if err := g.authorise(ctx, org, proposer.Describe(), capability); err != nil {
		return Draft{}, err
	}
	return proposer.Propose(ctx, bound(request))
}

// MaxRows bounds any single call.
//
// A ceiling this package imposes regardless of what a caller asks for. A
// connector returning a million rows is not an attack that needs to succeed to
// hurt — the memory is spent before anybody decides the answer is unreasonable.
const MaxRows = 500

// bound clamps a request to something this deployment will hold in memory.
func bound(r Request) Request {
	if r.Limit <= 0 || r.Limit > MaxRows {
		r.Limit = MaxRows
	}
	return r
}

// authorise is the check, and the only one.
func (g *Guard) authorise(
	ctx context.Context, org int64, descriptor Descriptor, capability string,
) error {
	if err := checkDescriptor(descriptor); err != nil {
		return err
	}

	grant, err := g.grants.GrantFor(ctx, org, descriptor.ID)
	if err != nil {
		return err
	}

	// The surface first. A connector whose tools have changed is not a
	// connector with a capability question — it is a different connector, and
	// asking whether it may do X presumes X still means what it meant.
	if grant.SurfaceDigest == "" {
		return fmt.Errorf("%w: %s has no pinned surface", ErrNotGranted, descriptor.ID)
	}
	presented := SurfaceDigest(descriptor)
	if presented != grant.SurfaceDigest {
		return fmt.Errorf(
			"%w: %s now offers a different set of tools (%s, granted against %s)",
			ErrSurfaceChanged, descriptor.ID,
			short(presented), short(grant.SurfaceDigest))
	}

	if !covers(grant.Capabilities, capability) {
		return fmt.Errorf("%w: %s may not %q here", ErrCapabilityNotGranted,
			descriptor.ID, capability)
	}

	// Spent last, so a call that was never going to be allowed does not consume
	// an allowance on its way to being refused.
	if !g.limiter.allow(org, descriptor.ID, grant.PerHour, g.now()) {
		return fmt.Errorf("%w: %s is limited to %d calls an hour here",
			ErrRateExceeded, descriptor.ID, grant.PerHour)
	}
	return nil
}

// checkDescriptor validates what a connector says about itself.
//
// Separate from CheckKind, which needs the concrete value to compare that claim
// against the interfaces it actually implements. This is what the guard can
// check when all it has is the descriptor.
func checkDescriptor(d Descriptor) error {
	switch d.Kind {
	case KindReader, KindProposer:
	default:
		return fmt.Errorf("connector: %q is not a kind of connector", d.Kind)
	}
	if d.ID == "" {
		return errors.New("connector: a connector must have an id to be granted anything")
	}
	return nil
}

// short is a digest prefix, for an error a person reads rather than compares.
func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12] + "…"
}

func covers(granted []string, capability string) bool {
	if capability == "" {
		return false
	}
	for _, g := range granted {
		if g == capability {
			return true
		}
	}
	return false
}

// SurfaceDigest is the hash a grant pins.
//
// Over the connector's id, its kind and its capabilities, sorted so that
// reordering a list does not read as a different surface — and so that two
// builds of the same connector agree.
//
// Length-prefixed for the same reason the credential binding is: without it, a
// connector offering ["read", "write"] and one offering ["readwrite"] would
// hash identically, and the second would inherit the first's grant.
func SurfaceDigest(d Descriptor) string {
	capabilities := append([]string(nil), d.Capabilities...)
	sort.Strings(capabilities)

	var b strings.Builder
	write := func(s string) {
		fmt.Fprintf(&b, "%d:%s", len(s), s)
	}
	write("stelfin-connector-surface-1")
	write(d.ID)
	write(string(d.Kind))
	fmt.Fprintf(&b, "%d:", len(capabilities))
	for _, c := range capabilities {
		write(c)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
