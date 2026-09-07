package settlement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// fakeRPC answers the three calls this package makes. Every submission status
// and both meanings of NOT_FOUND are reachable from here, which is the point:
// standing up an RPC node to prove that a DUPLICATE is not a failure would mean
// never proving it.
type fakeRPC struct {
	sim    protocol.SimulateTransactionResponse
	simErr error

	send    protocol.SendTransactionResponse
	sendErr error

	// gets is answered in order; the last entry repeats. A poll that has to see
	// NOT_FOUND and then SUCCESS is the ordinary case.
	gets    []protocol.GetTransactionResponse
	getErr  error
	getCall int

	sent []string
}

func (f *fakeRPC) SimulateTransaction(
	context.Context, protocol.SimulateTransactionRequest,
) (protocol.SimulateTransactionResponse, error) {
	return f.sim, f.simErr
}

func (f *fakeRPC) SendTransaction(
	_ context.Context, req protocol.SendTransactionRequest,
) (protocol.SendTransactionResponse, error) {
	f.sent = append(f.sent, req.Transaction)
	return f.send, f.sendErr
}

func (f *fakeRPC) GetTransaction(
	context.Context, protocol.GetTransactionRequest,
) (protocol.GetTransactionResponse, error) {
	if f.getErr != nil {
		return protocol.GetTransactionResponse{}, f.getErr
	}
	if len(f.gets) == 0 {
		return protocol.GetTransactionResponse{}, errors.New("fakeRPC: no reply configured")
	}
	i := f.getCall
	if i >= len(f.gets) {
		i = len(f.gets) - 1
	}
	f.getCall++
	return f.gets[i], nil
}

func sorobanClient(t *testing.T, rpc SorobanAPI) *Client {
	t.Helper()
	// accountFake rather than fakeHorizon: PrepareCall loads the source account
	// to build against, and everything else here must not submit.
	return testClient(&accountFake{}).WithSoroban(rpc)
}

// contractCall builds a minimal InvokeHostFunction envelope. The function it
// names does not matter — nothing here executes it, and what is under test is
// what stelfin does with the simulation's answer.
func contractCall(t *testing.T, extraOps ...txnbuild.Operation) *txnbuild.Transaction {
	t.Helper()
	source := keypair.MustRandom().Address()

	var contract xdr.ContractId
	for i := range contract {
		contract[i] = byte(i)
	}
	fn := xdr.HostFunction{
		Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		InvokeContract: &xdr.InvokeContractArgs{
			ContractAddress: xdr.ScAddress{
				Type:       xdr.ScAddressTypeScAddressTypeContract,
				ContractId: &contract,
			},
			FunctionName: "noop",
		},
	}

	ops := append([]txnbuild.Operation{
		&txnbuild.InvokeHostFunction{HostFunction: fn, SourceAccount: source},
	}, extraOps...)

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: source, Sequence: 41},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimebounds(0, 1_700_000_000),
		},
		Operations: ops,
	})
	if err != nil {
		t.Fatalf("build contract call: %v", err)
	}
	return tx
}

// footprint is a SorobanTransactionData carrying a resource fee, encoded the
// way simulation returns it.
func footprint(t *testing.T, resourceFee int64) string {
	t.Helper()
	data := xdr.SorobanTransactionData{ResourceFee: xdr.Int64(resourceFee)}
	encoded, err := xdr.MarshalBase64(data)
	if err != nil {
		t.Fatalf("encode footprint: %v", err)
	}
	return encoded
}

func TestSimulateReportsWhatTheCallCosts(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 12_345),
		MinResourceFee:     12_345,
	}}

	sim, err := sorobanClient(t, rpc).Simulate(context.Background(), contractCall(t))
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if sim.MinResourceFee != money.Stroops(12_345) {
		t.Errorf("MinResourceFee = %s", sim.MinResourceFee)
	}
}

// TestSimulateSurfacesAFailingCall: the call was tried against current ledger
// state and refused, at no cost and with no signature. Finding this out on
// chain instead means having paid for it.
func TestSimulateSurfacesAFailingCall(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		Error: "HostError: Error(Contract, #3)",
	}}

	_, err := sorobanClient(t, rpc).Simulate(context.Background(), contractCall(t))
	if !errors.Is(err, ErrSimulationFailed) {
		t.Fatalf("error = %v, want ErrSimulationFailed", err)
	}
	// The contract's own error has to survive, or nobody can debug it.
	if !strings.Contains(err.Error(), "Contract, #3") {
		t.Errorf("error loses the contract's reason: %v", err)
	}
}

// TestSimulateRefusesAHostileResourceFee is the ceiling that has no protocol
// equivalent. A contract with an attacker-controlled loop bound can make
// simulation report hundreds of XLM, and a client that trusted it would sign
// that bid.
func TestSimulateRefusesAHostileResourceFee(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, int64(MaxResourceFee)+1),
		MinResourceFee:     int64(MaxResourceFee) + 1,
	}}

	_, err := sorobanClient(t, rpc).Simulate(context.Background(), contractCall(t))
	if !errors.Is(err, ErrResourceFeeTooHigh) {
		t.Fatalf("error = %v, want ErrResourceFeeTooHigh", err)
	}
}

// TestSimulateRefusesArchivedState: restoring costs money, changes what the
// transaction does, and produces a different envelope from the one a person was
// shown. It is surfaced and left to a human.
func TestSimulateRefusesArchivedState(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 100),
		MinResourceFee:     100,
		RestorePreamble: &protocol.RestorePreamble{
			TransactionDataXDR: footprint(t, 500),
			MinResourceFee:     500,
		},
	}}

	_, err := sorobanClient(t, rpc).Simulate(context.Background(), contractCall(t))
	if !errors.Is(err, ErrArchivedState) {
		t.Fatalf("error = %v, want ErrArchivedState", err)
	}
}

// TestSorobanAllowsOneOperation: the footprint and the resource fee live on the
// transaction, not the operation, so two contract calls in one envelope cannot
// each carry their own. The SDK keeps the last one it saw and drops the other's
// footprint, producing an envelope that looks fine and fails on chain.
func TestSorobanAllowsOneOperation(t *testing.T) {
	rpc := &fakeRPC{sim: protocol.SimulateTransactionResponse{
		TransactionDataXDR: footprint(t, 100), MinResourceFee: 100,
	}}
	c := sorobanClient(t, rpc)

	twoOps := contractCall(t, &txnbuild.BumpSequence{BumpTo: 100})
	if _, err := c.Simulate(context.Background(), twoOps); !errors.Is(err, ErrNotSoroban) {
		t.Fatalf("two operations: %v, want ErrNotSoroban", err)
	}

	classic, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: keypair.MustRandom().Address(), Sequence: 1,
		},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 100}},
	})
	if err != nil {
		t.Fatalf("build classic: %v", err)
	}
	if _, err := c.Simulate(context.Background(), classic); !errors.Is(err, ErrNotSoroban) {
		t.Fatalf("a classic transaction: %v, want ErrNotSoroban", err)
	}
}

func TestSorobanNeedsAnEndpoint(t *testing.T) {
	c := testClient(&fakeHorizon{})
	if c.HasSoroban() {
		t.Fatal("a client with no RPC reports having one")
	}
	if _, err := c.Simulate(context.Background(), contractCall(t)); !errors.Is(err, ErrNoSoroban) {
		t.Errorf("Simulate: %v", err)
	}
	if _, err := c.SubmitSoroban(context.Background(), contractCall(t)); !errors.Is(err, ErrNoSoroban) {
		t.Errorf("SubmitSoroban: %v", err)
	}
}

// TestSubmissionStatusesAreFourDifferentFacts is the reason SubmitSoroban does
// not collapse them into ok/not-ok.
func TestSubmissionStatusesAreFourDifferentFacts(t *testing.T) {
	landed := protocol.GetTransactionResponse{
		TransactionDetails: protocol.TransactionDetails{
			Status: protocol.TransactionStatusSuccess, Ledger: 900,
		},
		LedgerCloseTime: 1_699_000_000,
	}

	t.Run("PENDING is queued, and we wait for it", func(t *testing.T) {
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending},
			gets: []protocol.GetTransactionResponse{landed},
		}
		got, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if err != nil {
			t.Fatalf("SubmitSoroban: %v", err)
		}
		if got.Ledger != 900 || got.Failed {
			t.Fatalf("result = %+v", got)
		}
	})

	t.Run("DUPLICATE is a correct retry, not a failure", func(t *testing.T) {
		// Treating this as an error is how a payment gets sent twice.
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusDuplicate},
			gets: []protocol.GetTransactionResponse{landed},
		}
		got, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if err != nil {
			t.Fatalf("SubmitSoroban: %v", err)
		}
		if got.Ledger != 900 {
			t.Fatalf("result = %+v", got)
		}
	})

	t.Run("TRY_AGAIN_LATER says nothing about the transaction", func(t *testing.T) {
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusTryAgainLater},
		}
		_, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if !errors.Is(err, ErrTryAgainLater) {
			t.Fatalf("error = %v, want ErrTryAgainLater", err)
		}
	})

	t.Run("ERROR is the only one where nothing happened", func(t *testing.T) {
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusError},
		}
		_, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("error = %v, want ErrRejected", err)
		}
	})
}

// TestAFailedSorobanTransactionIsNotANonEvent: it is on chain. It consumed the
// fee and the sequence number, so "success" would credit money that never moved
// and "error" would invite a retry against a sequence that has moved on.
func TestAFailedSorobanTransactionIsNotANonEvent(t *testing.T) {
	rpc := &fakeRPC{
		send: protocol.SendTransactionResponse{Status: stellarcore.TXStatusPending},
		gets: []protocol.GetTransactionResponse{{
			TransactionDetails: protocol.TransactionDetails{
				Status: protocol.TransactionStatusFailed, Ledger: 901,
			},
			LedgerCloseTime: 1_699_000_100,
		}},
	}

	got, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
	if err != nil {
		t.Fatalf("SubmitSoroban: %v", err)
	}
	if !got.Failed {
		t.Fatal("a FAILED transaction was reported as a success")
	}
	if got.Ledger != 901 || got.Hash == "" {
		t.Fatalf("result = %+v; a failed transaction still has a ledger and a hash", got)
	}
}

// TestNotFoundIsThreeDifferentAnswers. The status cannot tell them apart, so
// each is decided from something else.
func TestNotFoundIsThreeDifferentAnswers(t *testing.T) {
	notFound := func(latestClose int64, oldest uint32) protocol.GetTransactionResponse {
		return protocol.GetTransactionResponse{
			TransactionDetails:    protocol.TransactionDetails{Status: protocol.TransactionStatusNotFound},
			LatestLedgerCloseTime: latestClose,
			OldestLedger:          oldest,
			LatestLedger:          5000,
		}
	}
	landed := protocol.GetTransactionResponse{
		TransactionDetails: protocol.TransactionDetails{
			Status: protocol.TransactionStatusSuccess, Ledger: 4999,
		},
		LedgerCloseTime: 1_699_000_000,
	}

	t.Run("not included yet is worth waiting for", func(t *testing.T) {
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{
				Status: stellarcore.TXStatusPending, LatestLedger: 4000,
			},
			// Not found, not found, then there. The envelope's max time is
			// 1_700_000_000 and the ledger clock has not passed it.
			gets: []protocol.GetTransactionResponse{
				notFound(1_699_000_000, 3000),
				notFound(1_699_000_050, 3000),
				landed,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		got, err := sorobanClient(t, rpc).SubmitSoroban(ctx, contractCall(t))
		if err != nil {
			t.Fatalf("SubmitSoroban: %v", err)
		}
		if got.Ledger != 4999 {
			t.Fatalf("result = %+v", got)
		}
	})

	t.Run("past its time bounds it can never be included", func(t *testing.T) {
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{
				Status: stellarcore.TXStatusPending, LatestLedger: 4000,
			},
			gets: []protocol.GetTransactionResponse{notFound(1_700_000_001, 3000)},
		}
		_, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("outside the retained history it cannot say", func(t *testing.T) {
		// Sent at ledger 4000, and the RPC's history now starts at 4500. Its
		// "not found" is about its own retention, and concluding "did not
		// happen" here is how a transaction that landed gets sent twice.
		rpc := &fakeRPC{
			send: protocol.SendTransactionResponse{
				Status: stellarcore.TXStatusPending, LatestLedger: 4000,
			},
			gets: []protocol.GetTransactionResponse{notFound(1_699_000_000, 4500)},
		}
		_, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
		if !errors.Is(err, ErrRetentionGap) {
			t.Fatalf("error = %v, want ErrRetentionGap", err)
		}
	})
}

// TestAFailedSendIsNotAConclusion: the request failed, which says nothing about
// whether the transaction reached the network.
func TestAFailedSendIsNotAConclusion(t *testing.T) {
	rpc := &fakeRPC{
		sendErr: errors.New("connection reset"),
		gets: []protocol.GetTransactionResponse{{
			TransactionDetails: protocol.TransactionDetails{
				Status: protocol.TransactionStatusSuccess, Ledger: 777,
			},
			LedgerCloseTime: 1_699_000_000,
		}},
	}

	got, err := sorobanClient(t, rpc).SubmitSoroban(context.Background(), contractCall(t))
	if err != nil {
		t.Fatalf("SubmitSoroban: %v", err)
	}
	if got.Ledger != 777 {
		t.Fatalf("a transaction that landed despite a failed request was lost: %+v", got)
	}
}
