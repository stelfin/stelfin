package connector_test

import (
	"context"
	"errors"
	"go/build"
	"strings"
	"testing"
	"time"

	"github.com/stelfin/stelfin/connector"
)

// TestConnectorsCannotReachTheChain is the rule the package is built around.
//
// A connector that could build a transaction would be a connector that could
// decide one, and no amount of care downstream recovers from that. Enforced by
// reading the import graph rather than by intending it: this is exactly the
// kind of boundary that erodes through one convenient import at a time.
func TestConnectorsCannotReachTheChain(t *testing.T) {
	forbidden := map[string]string{
		"github.com/stelfin/stelfin/settlement":      "it could build and submit a transaction",
		"github.com/stellar/go-stellar-sdk/txnbuild": "it could build a transaction",
		"github.com/stellar/go-stellar-sdk/keypair":  "it could hold a key",
		"github.com/stelfin/stelfin/signer":          "it could ask for a signature",
	}

	seen := map[string]bool{}
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		if seen[path] || depth > 6 || !strings.HasPrefix(path, "github.com/stelfin/stelfin") {
			return
		}
		seen[path] = true

		pkg, err := build.Import(path, "", 0)
		if err != nil {
			return
		}
		for _, imported := range pkg.Imports {
			if why, bad := forbidden[imported]; bad {
				t.Errorf("%s imports %s — %s", path, imported, why)
			}
			walk(imported, depth+1)
		}
	}
	walk("github.com/stelfin/stelfin/connector", 0)

	if !seen["github.com/stelfin/stelfin/connector"] {
		t.Fatal("the package under test was not reached; this test is asserting nothing")
	}
}

// reader and proposer are the two shapes a connector may take.
type reader struct{ kind connector.Kind }

func (r reader) Describe() connector.Descriptor {
	return connector.Descriptor{ID: "sheet", Kind: r.kind}
}

func (reader) Read(context.Context, connector.Request) (connector.Observation, error) {
	return connector.Observation{ReadAt: time.Unix(1700000000, 0)}, nil
}

type proposer struct{ kind connector.Kind }

func (p proposer) Describe() connector.Descriptor {
	return connector.Descriptor{ID: "payroll", Kind: p.kind}
}

func (proposer) Propose(context.Context, connector.Request) (connector.Draft, error) {
	return connector.Draft{}, nil
}

// both is the shape that must not exist.
type both struct{}

func (both) Describe() connector.Descriptor {
	return connector.Descriptor{ID: "greedy", Kind: connector.KindReader}
}

func (both) Read(context.Context, connector.Request) (connector.Observation, error) {
	return connector.Observation{}, nil
}

func (both) Propose(context.Context, connector.Request) (connector.Draft, error) {
	return connector.Draft{}, nil
}

type neither struct{}

// TestAConnectorReadsOrProposes: one that could do both would let a read — the
// cheap, frequently granted capability — become a write through a code path
// somebody forgot to check.
func TestAConnectorReadsOrProposes(t *testing.T) {
	if err := connector.CheckKind(reader{kind: connector.KindReader}); err != nil {
		t.Errorf("a reader was refused: %v", err)
	}
	if err := connector.CheckKind(proposer{kind: connector.KindProposer}); err != nil {
		t.Errorf("a proposer was refused: %v", err)
	}

	if err := connector.CheckKind(both{}); !errors.Is(err, connector.ErrBothKinds) {
		t.Errorf("a connector that does both was accepted: %v", err)
	}
	if err := connector.CheckKind(neither{}); err == nil {
		t.Error("a connector that does neither was accepted")
	}
}

// TestTheDescriptorMustMatchTheInterface: the descriptor is the connector's own
// claim about itself and the interface is the fact. A grant recorded against
// "reader" must not be authorising something that proposes.
func TestTheDescriptorMustMatchTheInterface(t *testing.T) {
	if err := connector.CheckKind(reader{kind: connector.KindProposer}); !errors.Is(
		err, connector.ErrBothKinds,
	) {
		t.Errorf("a reader calling itself a proposer was accepted: %v", err)
	}
	if err := connector.CheckKind(proposer{kind: connector.KindReader}); !errors.Is(
		err, connector.ErrBothKinds,
	) {
		t.Errorf("a proposer calling itself a reader was accepted: %v", err)
	}
}

// TestUntrustedHasToBeUnwrappedDeliberately.
//
// The compile-time half cannot be asserted from a test — an Untrusted[string]
// passed where a string is wanted does not build, which is the point. What is
// asserted here is that the value survives intact, so the wrapper is a marker
// and not a transformation somebody would be tempted to route around.
func TestUntrustedHasToBeUnwrappedDeliberately(t *testing.T) {
	const raw = "  =SUM(A1:A9)  "
	u := connector.Wrap(raw)
	if u.Unwrap() != raw {
		t.Fatalf("Unwrap changed the value to %q", u.Unwrap())
	}

	// The zero value is usable and empty, so a row missing a column reads as
	// blank rather than panicking somewhere later.
	var empty connector.Untrusted[string]
	if empty.Unwrap() != "" {
		t.Errorf("the zero value is %q", empty.Unwrap())
	}
}
