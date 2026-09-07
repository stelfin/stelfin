package settlement

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

const testContractAddress = "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE"

func preparedClient(t *testing.T, resourceFee int64) *Client {
	t.Helper()
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, resourceFee),
		MinResourceFee:     resourceFee,
	}}
	return sorobanClient(t, rpc)
}

func TestPrepareCallBuildsSimulatesAndAssembles(t *testing.T) {
	c := preparedClient(t, 40_000)
	source := keypair.MustRandom().Address()

	to, err := ScAddress(keypair.MustRandom().Address())
	if err != nil {
		t.Fatalf("ScAddress: %v", err)
	}

	prepared, err := c.PrepareCall(context.Background(), CallRequest{
		Source:   source,
		Contract: testContractAddress,
		Function: "transfer",
		Args:     []xdr.ScVal{to, ScAmount(money.MustParse("250"))},
	})
	if err != nil {
		t.Fatalf("PrepareCall: %v", err)
	}

	// The footprint has to be on the envelope before anyone signs it: a
	// signature over a transaction without one covers something the network
	// will reject.
	if prepared.Transaction.ToXDR().V1.Tx.Ext.SorobanData == nil {
		t.Fatal("the prepared call carries no footprint")
	}
	if prepared.Simulation.MinResourceFee != money.Stroops(40_000) {
		t.Errorf("MinResourceFee = %s", prepared.Simulation.MinResourceFee)
	}

	// And it is describable, because a call this deployment cannot render must
	// not reach a page that has to show it.
	d, err := c.DescribeTx(prepared.Transaction)
	if err != nil {
		t.Fatalf("DescribeTx: %v", err)
	}
	if len(d.Operations) != 1 || d.Operations[0].Type != "invoke_contract" {
		t.Fatalf("described %+v", d.Operations)
	}
	if !strings.Contains(d.Operations[0].Summary, "transfer") {
		t.Errorf("summary does not name the function: %q", d.Operations[0].Summary)
	}
}

// TestPrepareCallRefusesBeforeItReachesTheNetwork: a bad function name or
// address is a wasted round trip at best, and a name with a character the
// description escapes is a name somebody could make render as another.
func TestPrepareCallRefusesBeforeItReachesTheNetwork(t *testing.T) {
	c := preparedClient(t, 100)
	good := CallRequest{
		Source:   keypair.MustRandom().Address(),
		Contract: testContractAddress,
		Function: "transfer",
	}

	cases := map[string]func(CallRequest) CallRequest{
		"not an account":     func(r CallRequest) CallRequest { r.Source = "nope"; return r },
		"contract as source": func(r CallRequest) CallRequest { r.Source = testContractAddress; return r },
		"account as contract": func(r CallRequest) CallRequest {
			r.Contract = keypair.MustRandom().Address()
			return r
		},
		"empty function": func(r CallRequest) CallRequest { r.Function = ""; return r },
		"function with a dot": func(r CallRequest) CallRequest {
			r.Function = "trans.fer"
			return r
		},
		"function starting with a digit": func(r CallRequest) CallRequest {
			r.Function = "1transfer"
			return r
		},
		"function too long": func(r CallRequest) CallRequest {
			r.Function = strings.Repeat("a", 33)
			return r
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.PrepareCall(context.Background(), mutate(good)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestPrepareCallKeepsCollectedAuthorisation: entries somebody signed are not
// interchangeable with entries simulation recorded.
func TestPrepareCallKeepsCollectedAuthorisation(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 100),
		MinResourceFee:     100,
		Results: []protocol.SimulateHostFunctionResult{
			{AuthXDR: &[]string{authEntry(t, 99)}},
		},
	}}
	c := sorobanClient(t, rpc)

	var supplied xdr.SorobanAuthorizationEntry
	if err := xdr.SafeUnmarshalBase64(authEntry(t, 7), &supplied); err != nil {
		t.Fatalf("decode: %v", err)
	}

	prepared, err := c.PrepareCall(context.Background(), CallRequest{
		Source:   keypair.MustRandom().Address(),
		Contract: testContractAddress,
		Function: "burn",
		Auth:     []xdr.SorobanAuthorizationEntry{supplied},
	})
	if err != nil {
		t.Fatalf("PrepareCall: %v", err)
	}

	auth := prepared.Transaction.Operations()[0].(*txnbuild.InvokeHostFunction).Auth
	if len(auth) != 1 {
		t.Fatalf("%d auth entries", len(auth))
	}
	if auth[0].RootInvocation.Function.ContractFn.ContractAddress.ContractId[0] != 7 {
		t.Fatal("collected authorisation was replaced by a recorded one")
	}
}

func TestPrepareCallNeedsAnEndpoint(t *testing.T) {
	c := testClient(&fakeHorizon{})
	if _, err := c.PrepareCall(context.Background(), CallRequest{
		Source: keypair.MustRandom().Address(), Contract: testContractAddress, Function: "x",
	}); !errors.Is(err, ErrNoSoroban) {
		t.Fatalf("error = %v, want ErrNoSoroban", err)
	}
}

// TestScAmountIsExactAtEveryScale is the conversion that decides quantity. The
// wrong integer width is a payment for the wrong amount that every layer
// downstream reports as correct.
func TestScAmountIsExactAtEveryScale(t *testing.T) {
	cases := map[string]struct {
		amount money.Stroops
		want   string
	}{
		"zero":            {0, "i128:0"},
		"one stroop":      {1, "i128:1"},
		"a round amount":  {money.MustParse("250"), "i128:2500000000"},
		"a fraction":      {money.MustParse("0.0000001"), "i128:1"},
		"negative":        {-1, "i128:-1"},
		"int64 maximum":   {money.Stroops(math.MaxInt64), "i128:9223372036854775807"},
		"int64 minimum":   {money.Stroops(math.MinInt64), "i128:-9223372036854775808"},
		"negative amount": {-money.MustParse("1.5"), "i128:-15000000"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := scvalString(ScAmount(tc.amount))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ScAmount(%d) = %s, want %s", int64(tc.amount), got, tc.want)
			}
		})
	}
}

func TestScAddressAndScSymbol(t *testing.T) {
	account := keypair.MustRandom().Address()

	got, err := ScAddress(account)
	if err != nil {
		t.Fatalf("ScAddress: %v", err)
	}
	if rendered, _ := scvalString(got); rendered != "addr:"+account {
		t.Errorf("account rendered as %q", rendered)
	}

	got, err = ScAddress(testContractAddress)
	if err != nil {
		t.Fatalf("ScAddress: %v", err)
	}
	if rendered, _ := scvalString(got); rendered != "addr:"+testContractAddress {
		t.Errorf("contract rendered as %q", rendered)
	}

	if _, err := ScAddress("neither"); err == nil {
		t.Error("accepted something that is neither")
	}

	if _, err := ScSymbol("transfer"); err != nil {
		t.Errorf("ScSymbol: %v", err)
	}
	// A symbol carrying a delimiter is a symbol that could be made to render as
	// two values, so it is refused at construction rather than escaped away.
	for _, bad := range []string{"", "a,b", "a=b", strings.Repeat("x", 33), "1abc"} {
		if _, err := ScSymbol(bad); err == nil {
			t.Errorf("accepted symbol %q", bad)
		}
	}
}
