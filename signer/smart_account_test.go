package signer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/signer"
)

// A real contract address, so the strkey check is exercised rather than
// stubbed out by a placeholder that would fail for the wrong reason.
const testContract = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"

type checker struct {
	ok       bool
	err      error
	contract string
	calls    int
}

func (c *checker) CheckAuth(_ context.Context, contract string, _ signer.Request) (bool, error) {
	c.calls++
	c.contract = contract
	return c.ok, c.err
}

func TestSmartAccountWithoutASimulatorRefusesToGuess(t *testing.T) {
	a := keyFor(t, 20)

	_, err := signer.NewSmartAccount(testContract, nil).Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: testContract,
		Tx:      payment(t, a.Address()),
	})
	// Deliberately an error and not Complete=false. "Not yet complete" tells a
	// caller to wait for a signature, and for a contract account no signature
	// is ever coming, so that wait would never end.
	if !errors.Is(err, signer.ErrAuthUndecidable) {
		t.Fatalf("got %v, want ErrAuthUndecidable", err)
	}
}

func TestSmartAccountAsksTheContract(t *testing.T) {
	a := keyFor(t, 21)

	for _, authorised := range []bool{true, false} {
		c := &checker{ok: authorised}
		got, err := signer.NewSmartAccount(testContract, c).Sign(
			context.Background(), signer.Request{
				Purpose: signer.PurposeTreasury,
				Network: testNet,
				Account: testContract,
				Tx:      payment(t, a.Address()),
			})
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if got.Complete != authorised {
			t.Errorf("Complete = %v, want %v", got.Complete, authorised)
		}
		if c.calls != 1 || c.contract != testContract {
			t.Errorf("checker called %d time(s) for %q, want 1 for %q",
				c.calls, c.contract, testContract)
		}
		// There is no threshold to be part of the way towards, so anything
		// rendering "1 of 2" from these would be inventing it.
		if got.Have != 0 || got.Need != 0 {
			t.Errorf("Have/Need = %d/%d, want 0/0 for a contract account", got.Have, got.Need)
		}
	}
}

func TestSmartAccountRejectsAnAccountThatIsNotAContract(t *testing.T) {
	a := keyFor(t, 22)

	// A G-address is a perfectly valid Stellar account and completely wrong
	// here: it has thresholds, and routing it through contract simulation
	// would answer a question nobody asked.
	s := signer.NewSmartAccount(a.Address(), &checker{ok: true})
	if _, err := s.Address(context.Background()); err == nil {
		t.Error("Address accepted a G-address as a contract")
	}
	if _, err := s.Sign(context.Background(), signer.Request{
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: a.Address(),
		Tx:      payment(t, a.Address()),
	}); err == nil {
		t.Error("Sign accepted a G-address as a contract")
	}
}

func TestRouterSendsAContractTreasuryToTheSimulator(t *testing.T) {
	operator := keyFor(t, 23)
	local, err := signer.NewLocal(operator.Seed(), testNet, false)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	c := &checker{ok: true}
	r, err := signer.NewRouter(signer.Config{
		Network:  testNet,
		Operator: local,
		// Signers and Medium are present and must be ignored: once Contract is
		// set, the contract decides and weight arithmetic means nothing.
		Treasuries: lookup{set: signer.Set{
			Signers:  map[string]int32{operator.Address(): 5},
			Medium:   1,
			Contract: testContract,
		}},
		Auth: c,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	got, err := r.Sign(context.Background(), signer.Request{
		Org:     ledger.OrgID(1),
		Purpose: signer.PurposeTreasury,
		Network: testNet,
		Account: testContract,
		Tx:      payment(t, operator.Address()),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("checker called %d time(s), want 1", c.calls)
	}
	if !got.Complete {
		t.Error("Complete = false although the contract authorised")
	}
	if n := len(got.Tx.Signatures()); n != 0 {
		t.Fatalf("the router added %d signature(s) to a contract treasury's envelope", n)
	}
}
