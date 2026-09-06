package settlement

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// authEntry is a recorded authorisation entry, encoded the way simulation
// returns it. What it authorises does not matter here; whose it is does.
func authEntry(t *testing.T, nonce int64) string {
	t.Helper()
	var contract xdr.ContractId
	contract[0] = byte(nonce)

	entry := xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount,
		},
		RootInvocation: xdr.SorobanAuthorizedInvocation{
			Function: xdr.SorobanAuthorizedFunction{
				Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
				ContractFn: &xdr.InvokeContractArgs{
					ContractAddress: xdr.ScAddress{
						Type:       xdr.ScAddressTypeScAddressTypeContract,
						ContractId: &contract,
					},
					FunctionName: "noop",
				},
			},
		},
	}
	encoded, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("encode auth entry: %v", err)
	}
	return encoded
}

func TestAssembleWritesTheFootprintAndTheFee(t *testing.T) {
	c := testClient(&fakeHorizon{})
	tx := contractCall(t)

	assembled, err := c.Assemble(tx, &Simulation{
		TransactionData: footprint(t, 90_000),
		MinResourceFee:  90_000,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	envelope := assembled.ToXDR()
	data := envelope.V1.Tx.Ext.SorobanData
	if data == nil {
		t.Fatal("the assembled envelope carries no footprint; the network would reject it")
	}
	if int64(data.ResourceFee) != 90_000 {
		t.Errorf("resource fee = %d, want 90000", data.ResourceFee)
	}
	// The bid is the ordinary per-operation fee plus what the resources cost.
	// Getting this wrong underbids and the transaction never lands.
	if want := c.baseFee + 90_000; assembled.MaxFee() != want {
		t.Errorf("max fee = %d, want %d", assembled.MaxFee(), want)
	}
}

// TestAssembleKeepsTheSequenceItSimulatedAgainst: the envelope already holds
// the sequence it reserves. Incrementing again would attach a footprint
// simulated at N to a transaction valid only at N+1, which can never be
// included.
func TestAssembleKeepsTheSequenceItSimulatedAgainst(t *testing.T) {
	c := testClient(&fakeHorizon{})
	tx := contractCall(t)
	before := tx.SequenceNumber()

	assembled, err := c.Assemble(tx, &Simulation{TransactionData: footprint(t, 100)})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if assembled.SequenceNumber() != before {
		t.Fatalf("sequence moved from %d to %d", before, assembled.SequenceNumber())
	}
	if assembled.SourceAccount().AccountID != tx.SourceAccount().AccountID {
		t.Errorf("source account changed")
	}
	// The signing window has to survive too, or the envelope people are
	// collecting signatures against expires at a different moment than the one
	// they were told.
	if assembled.Timebounds().MaxTime != tx.Timebounds().MaxTime {
		t.Errorf("time bounds changed: %d then %d",
			tx.Timebounds().MaxTime, assembled.Timebounds().MaxTime)
	}
}

// TestAssembleNeverOverwritesSuppliedAuth is the rule with the sharpest edge.
//
// Simulation records auth by pretending every signature it needs is present.
// That is a convenience for the case where the source account authorises its
// own call — it is not a statement that anyone agreed. Replacing entries
// somebody actually signed with recorded ones substitutes "the network would
// accept this if these people agreed" for "these people agreed".
func TestAssembleNeverOverwritesSuppliedAuth(t *testing.T) {
	c := testClient(&fakeHorizon{})

	var supplied []xdr.SorobanAuthorizationEntry
	var entry xdr.SorobanAuthorizationEntry
	if err := xdr.SafeUnmarshalBase64(authEntry(t, 7), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	supplied = append(supplied, entry)

	tx := contractCall(t)
	invoke := tx.Operations()[0].(*txnbuild.InvokeHostFunction)
	invoke.Auth = supplied

	assembled, err := c.Assemble(tx, &Simulation{
		TransactionData: footprint(t, 100),
		// Simulation offers different entries. They must be ignored.
		Auth: []string{authEntry(t, 99), authEntry(t, 100)},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	got := assembled.Operations()[0].(*txnbuild.InvokeHostFunction).Auth
	if len(got) != 1 {
		t.Fatalf("%d auth entries, want the one that was supplied", len(got))
	}
	fn := got[0].RootInvocation.Function.ContractFn
	if fn == nil || fn.ContractAddress.ContractId[0] != 7 {
		t.Fatalf("the supplied authorisation was replaced by a recorded one")
	}
}

// TestAssembleUsesRecordedAuthWhenThereIsNone: the ordinary case, where the
// source account authorises its own call and nobody else is involved.
func TestAssembleUsesRecordedAuthWhenThereIsNone(t *testing.T) {
	c := testClient(&fakeHorizon{})

	assembled, err := c.Assemble(contractCall(t), &Simulation{
		TransactionData: footprint(t, 100),
		Auth:            []string{authEntry(t, 3)},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	got := assembled.Operations()[0].(*txnbuild.InvokeHostFunction).Auth
	if len(got) != 1 {
		t.Fatalf("%d auth entries, want the recorded one", len(got))
	}
}

// TestAssembleRefusesASignedEnvelope: assembling changes the bytes a signature
// covers, so doing it silently leaves a caller holding approvals that no longer
// approve anything.
func TestAssembleRefusesASignedEnvelope(t *testing.T) {
	c := testClient(&fakeHorizon{})
	kp := keypair.MustRandom()

	signed, err := contractCall(t).Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := c.Assemble(signed, &Simulation{TransactionData: footprint(t, 100)}); err == nil {
		t.Fatal("a signed envelope was reassembled, invalidating its signature in silence")
	}
}

// TestAssembleRefusesAFootprintOverTheCeiling: Simulate applied the ceiling to
// the number simulation reported. This applies it to the number that will
// actually be bid, so the two cannot disagree.
func TestAssembleRefusesAFootprintOverTheCeiling(t *testing.T) {
	c := testClient(&fakeHorizon{})

	_, err := c.Assemble(contractCall(t), &Simulation{
		TransactionData: footprint(t, int64(MaxResourceFee)+1),
		// Deliberately a modest number here: a caller trusting the reported
		// figure over the footprint's own would let this through.
		MinResourceFee: 100,
	})
	if !errors.Is(err, ErrResourceFeeTooHigh) {
		t.Fatalf("error = %v, want ErrResourceFeeTooHigh", err)
	}
}

func TestAssembleRefusesWhatIsNotAContractCall(t *testing.T) {
	c := testClient(&fakeHorizon{})

	if _, err := c.Assemble(contractCall(t, &txnbuild.BumpSequence{BumpTo: 5}),
		&Simulation{TransactionData: footprint(t, 100)}); !errors.Is(err, ErrNotSoroban) {
		t.Errorf("two operations: %v", err)
	}
	if _, err := c.Assemble(contractCall(t), nil); err == nil {
		t.Error("assembled without a simulation")
	}
}
