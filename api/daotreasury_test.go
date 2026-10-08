package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/protocols/stellarcore"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/contracts/treasuryabi"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// treasuryCode is the hash the tests' deployment was "built with".
var treasuryCode = strings.Repeat("7e", 32)

// chain is a Soroban RPC that plays a treasury contract by rote.
//
// It answers a simulation by the name of the function being called, with
// whatever the test said that function returns or refuses with; it accepts
// what it is sent and reports it landed. It does none of the contract's
// reasoning — that is the contract's own suite's to prove — and exists so that
// what this package builds, records and sends can be looked at.
type chain struct {
	codeOnChain

	// returns is what each function answers, by name.
	returns map[string]xdr.ScVal
	// refuses is the contract error code each function fails with, by name.
	refuses map[string]uint32

	// simulated is every call asked about, as "function(arg,arg)".
	simulated []string
	// contracts is which contract each of those was made to.
	contracts []string
	// creations is how many contracts were asked to be created.
	creations int

	sendStatus string
	sent       []string
	failed     bool
	landedWith *xdr.ScVal
}

func newChain() *chain {
	return &chain{
		codeOnChain: codeOnChain{wasm: treasuryCode},
		returns:     map[string]xdr.ScVal{},
		refuses:     map[string]uint32{},
		sendStatus:  stellarcore.TXStatusPending,
	}
}

func (c *chain) SimulateTransaction(
	_ context.Context, req protocol.SimulateTransactionRequest,
) (protocol.SimulateTransactionResponse, error) {
	var envelope xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(req.Transaction, &envelope); err != nil {
		return protocol.SimulateTransactionResponse{}, err
	}
	fn := envelope.V1.Tx.Operations[0].Body.InvokeHostFunctionOp.HostFunction

	data, _ := xdr.MarshalBase64(xdr.SorobanTransactionData{ResourceFee: 1_000})
	respond := func(v xdr.ScVal) (protocol.SimulateTransactionResponse, error) {
		encoded, err := xdr.MarshalBase64(v)
		return protocol.SimulateTransactionResponse{
			TransactionDataXDR: data, MinResourceFee: 1_000,
			Results: []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &encoded}},
		}, err
	}

	if fn.Type == xdr.HostFunctionTypeHostFunctionTypeCreateContractV2 {
		c.creations++
		if code, refused := c.refuses["__constructor"]; refused {
			return protocol.SimulateTransactionResponse{
				Error: fmt.Sprintf("HostError: Error(Contract, #%d)", code),
			}, nil
		}
		created, _ := treasuryabi.Address(createdAt)
		return respond(created)
	}

	call := fn.InvokeContract
	name := string(call.FunctionName)
	contract, _ := strkey.Encode(strkey.VersionByteContract, call.ContractAddress.ContractId[:])
	c.simulated = append(c.simulated, name)
	c.contracts = append(c.contracts, contract)

	if code, refused := c.refuses[name]; refused {
		return protocol.SimulateTransactionResponse{
			Error: fmt.Sprintf("HostError: Error(Contract, #%d)\n\nEvent log (newest first): …", code),
		}, nil
	}
	if v, ok := c.returns[name]; ok {
		return respond(v)
	}
	return respond(xdr.ScVal{Type: xdr.ScValTypeScvVoid})
}

func (c *chain) SendTransaction(
	_ context.Context, req protocol.SendTransactionRequest,
) (protocol.SendTransactionResponse, error) {
	c.sent = append(c.sent, req.Transaction)
	return protocol.SendTransactionResponse{Status: c.sendStatus}, nil
}

func (c *chain) GetTransaction(
	context.Context, protocol.GetTransactionRequest,
) (protocol.GetTransactionResponse, error) {
	details := protocol.TransactionDetails{Status: protocol.TransactionStatusSuccess, Ledger: 4242}
	if c.failed {
		details.Status = protocol.TransactionStatusFailed
	}
	if c.landedWith != nil {
		details.ResultMetaXDR, _ = xdr.MarshalBase64(xdr.TransactionMeta{
			V: 3, V3: &xdr.TransactionMetaV3{SorobanMeta: &xdr.SorobanTransactionMeta{ReturnValue: *c.landedWith}},
		})
	}
	return protocol.GetTransactionResponse{TransactionDetails: details}, nil
}

// anotherContract is a contract address nothing else in this suite has used.
// One address belongs to one workspace, so each fixture needs its own.
func anotherContract() string {
	raw, _ := strkey.Decode(strkey.VersionByteAccountID, keypair.MustRandom().Address())
	address, _ := strkey.Encode(strkey.VersionByteContract, raw)
	return address
}

// createdAt is where the fake says a new contract will be.
var createdAt = func() string {
	raw := make([]byte, 32)
	copy(raw, []byte("the treasury this test created!"))
	address, _ := strkey.Encode(strkey.VersionByteContract, raw)
	return address
}()

// daoFixture is a workspace with a treasury held by a contract and one member
// whose wallet the test holds the key to.
type daoFixture struct {
	*proposalFixture
	chain    *chain
	relay    *keypair.Full
	wallet   *keypair.Full
	who      store.Member
	treasury store.Treasury
}

func newDaoFixture(t *testing.T, name string) *daoFixture {
	t.Helper()
	ctx := context.Background()
	f := newProposalFixture(t, name)

	rpc := newChain()
	relay := keypair.MustRandom()
	f.svc.settle = f.svc.settle.WithSoroban(rpc)
	f.svc.cfg.Treasuries = &ContractTreasuryConfig{
		WasmHash:     treasuryCode,
		RelayAddress: relay.Address(),
		SignRelay: func(tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
			return tx.Sign(network.TestNetworkPassphrase, relay)
		},
	}

	wallet := keypair.MustRandom()
	if err := f.store.SetMemberAddress(ctx, f.scope.Org, f.member, wallet.Address(), store.AddressLinked); err != nil {
		t.Fatalf("set address: %v", err)
	}
	who, err := f.store.Member(ctx, f.scope.Org, f.member)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	treasury, err := f.store.AddContractTreasury(ctx, f.scope.Org, anotherContract(), "ops", f.identity)
	if err != nil {
		t.Fatalf("add treasury: %v", err)
	}
	return &daoFixture{proposalFixture: f, chain: rpc, relay: relay, wallet: wallet, who: who, treasury: treasury}
}

func (f *daoFixture) signFeeBump(tx *txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error) {
	return tx.Sign(network.TestNetworkPassphrase, f.treasuryFixture.treasury)
}

func payTo(t *testing.T, f *daoFixture, to string) treasuryabi.Action {
	t.Helper()
	action, err := f.svc.PayAction(txnbuild.NativeAsset{}, to, money.MustParse("250"))
	if err != nil {
		t.Fatalf("PayAction: %v", err)
	}
	return action
}

// The whole of what a member's call goes through: built from their address,
// shown back from the envelope, accepted only signed, sent inside the
// operator's fee bump, and sent once.
func TestAMembersCallIsBuiltShownSignedAndSentOnce(t *testing.T) {
	f := newDaoFixture(t, "member call")
	ctx := context.Background()
	to := keypair.MustRandom().Address()
	action := payTo(t, f, to)

	prepared, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, action, "  March invoice ")
	if err != nil {
		t.Fatalf("ProposeOnChain: %v", err)
	}
	if got := fmt.Sprint(f.chain.simulated); got != "[propose]" {
		t.Fatalf("simulated %s before asking anybody to sign", got)
	}

	view, err := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash)
	if err != nil {
		t.Fatalf("LoadMemberCall: %v", err)
	}
	if view.Call.Source != f.wallet.Address() || view.Description.Source != f.wallet.Address() {
		t.Errorf("the call is from %s, and the member's wallet is %s", view.Description.Source, f.wallet.Address())
	}
	if view.Call.Contract != f.treasury.Address || view.Call.Function != "propose" {
		t.Errorf("recorded as %s on %s", view.Call.Function, view.Call.Contract)
	}
	if view.Call.Note != "March invoice" || !strings.Contains(view.Call.Says, "pay 250.00 XLM to "+to) {
		t.Errorf("says %q, note %q", view.Call.Says, view.Call.Note)
	}
	op := view.Description.Operations[0]
	if op.Type != "invoke_contract" || !strings.Contains(op.Summary, "propose") {
		t.Errorf("described as %+v", op)
	}

	// Unsigned, it is not accepted, and the attempt does not use it up.
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, view.Call.XDR, f.treasuryFixture.treasury.Address(), f.signFeeBump); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("an unsigned call: %v", err)
	}

	f.chain.landedWith = ptrTo(treasuryabi.U32(3))
	signed := signEnvelope(t, view.Call.XDR, f.wallet)
	res, err := f.svc.SubmitMemberCall(ctx, f.scope, signed, f.treasuryFixture.treasury.Address(), f.signFeeBump)
	if err != nil {
		t.Fatalf("SubmitMemberCall: %v", err)
	}
	if res.Proposal != 3 || res.Function != "propose" || res.Ledger != 4242 {
		t.Errorf("result %+v", res)
	}

	// What went out is a fee bump around the member's own transaction.
	if len(f.chain.sent) != 1 {
		t.Fatalf("sent %d envelope(s)", len(f.chain.sent))
	}
	parsed, err := txnbuild.TransactionFromXDR(f.chain.sent[0])
	if err != nil {
		t.Fatalf("what was sent: %v", err)
	}
	bump, isBump := parsed.FeeBump()
	if !isBump {
		t.Fatal("the member's call went out without the envelope that pays for it")
	}
	inner, _ := bump.InnerTransaction().HashHex(network.TestNetworkPassphrase)
	if inner != prepared.Hash || bump.FeeAccount() != f.treasuryFixture.treasury.Address() {
		t.Errorf("fee bump by %s around %s", bump.FeeAccount(), inner)
	}

	// Once. The same envelope again is refused, and so is looking at it.
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, signed, f.treasuryFixture.treasury.Address(), f.signFeeBump); !errors.Is(err, store.ErrCallSubmitted) {
		t.Errorf("a second submission: %v", err)
	}
	if _, err := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash); !errors.Is(err, store.ErrCallSubmitted) {
		t.Errorf("loading a call already sent: %v", err)
	}
	if len(f.chain.sent) != 1 {
		t.Errorf("a refused resubmission was sent anyway (%d envelopes)", len(f.chain.sent))
	}
}

func ptrTo[T any](v T) *T { return &v }

// A call belongs to whoever it was built for. To anybody else it does not
// exist, which is a different answer from "not yours" on purpose.
func TestACallIsOnlyItsOwners(t *testing.T) {
	f := newDaoFixture(t, "call owner")
	ctx := context.Background()
	prepared, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who,
		payTo(t, f, keypair.MustRandom().Address()), "")
	if err != nil {
		t.Fatalf("ProposeOnChain: %v", err)
	}
	view, err := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash)
	if err != nil {
		t.Fatal(err)
	}
	signed := signEnvelope(t, view.Call.XDR, f.wallet)

	stranger := Scope{Org: f.scope.Org, OwnerRef: "telegram:somebody-else"}
	if _, err := f.svc.LoadMemberCall(ctx, stranger, prepared.Hash); !errors.Is(err, store.ErrNoMemberCall) {
		t.Errorf("somebody else loading it: %v", err)
	}
	if _, err := f.svc.SubmitMemberCall(ctx, stranger, signed, f.treasuryFixture.treasury.Address(), f.signFeeBump); !errors.Is(err, store.ErrCallNotYours) {
		t.Errorf("somebody else submitting it: %v", err)
	}
	elsewhere := Scope{Org: orgFor(t, "call owner elsewhere").ID, OwnerRef: f.scope.OwnerRef}
	if _, err := f.svc.SubmitMemberCall(ctx, elsewhere, signed, f.treasuryFixture.treasury.Address(), f.signFeeBump); !errors.Is(err, store.ErrNoMemberCall) {
		t.Errorf("the same person in another workspace: %v", err)
	}
	// And an envelope this server never built is nobody's.
	other := signEnvelope(t, f.propose(t).Proposal.XDR, f.wallet)
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, other, f.treasuryFixture.treasury.Address(), f.signFeeBump); !errors.Is(err, store.ErrNoMemberCall) {
		t.Errorf("an envelope that is not a call: %v", err)
	}
	if len(f.chain.sent) != 0 {
		t.Errorf("%d envelope(s) were sent on the operator's money", len(f.chain.sent))
	}
}

// What the contract would refuse is refused before anybody signs, with the
// contract's own reason, and nothing is left behind for a link to point at.
func TestWhatTheContractRefusesIsRefusedBeforeSigning(t *testing.T) {
	f := newDaoFixture(t, "refused")
	ctx := context.Background()
	f.chain.refuses["propose"] = uint32(treasuryabi.RecipientNotAllowed)

	_, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, payTo(t, f, keypair.MustRandom().Address()), "")
	var refusal *ContractRefusal
	if !errors.As(err, &refusal) || refusal.Code != treasuryabi.RecipientNotAllowed {
		t.Fatalf("got %v, want the contract's refusal", err)
	}
	var rows int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM member_calls WHERE org_id = $1`, int64(f.scope.Org)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d call(s) were recorded for something the contract refused", rows)
	}

	// A deposit is a call to the token, and a token's refusal is not read off
	// the treasury's list: its code 10 is not "over the spending limit".
	f.chain.refuses["transfer"] = 10
	_, err = f.svc.DepositOnChain(ctx, f.scope, f.treasury, f.who, txnbuild.NativeAsset{}, money.MustParse("5"))
	var token *TokenRefusal
	if !errors.As(err, &token) || token.Code != 10 || errors.As(err, &refusal) {
		t.Errorf("a token's refusal came back as %v", err)
	}
}

// A deposit calls the token's contract, from the member, into the treasury —
// and the token is the one derived from the asset, not one anybody supplied.
func TestADepositIsATransferTheMemberMakes(t *testing.T) {
	f := newDaoFixture(t, "deposit")
	ctx := context.Background()

	prepared, err := f.svc.DepositOnChain(ctx, f.scope, f.treasury, f.who, txnbuild.NativeAsset{}, money.MustParse("5"))
	if err != nil {
		t.Fatalf("DepositOnChain: %v", err)
	}
	view, err := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash)
	if err != nil {
		t.Fatal(err)
	}
	const nativeOnTestnet = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	if view.Call.Contract != nativeOnTestnet || view.Call.Function != "transfer" {
		t.Fatalf("a deposit of XLM calls %s on %s", view.Call.Function, view.Call.Contract)
	}
	want := map[string]string{
		"contract": nativeOnTestnet,
		"arg 0":    "addr:" + f.wallet.Address(),
		"arg 1":    "addr:" + f.treasury.Address,
		"arg 2":    "i128:50000000",
	}
	for _, field := range view.Description.Operations[0].Fields {
		if expected, ok := want[field.Label]; ok && field.Value != expected {
			t.Errorf("%s is %s, want %s", field.Label, field.Value, expected)
		}
	}
	// The page is told which assets a token might be, so it can work out for
	// itself which one this is.
	if len(view.Assets) != 2 || !view.Assets[0].IsNative() || view.Assets[1].GetCode() != "USDC" {
		t.Errorf("asset hints: %v", view.Assets)
	}
}

// A call the network turned away before a ledger cost nothing, and can be
// offered again. One that reached a ledger and failed there cannot.
func TestARejectedCallCanBeOfferedAgainAndAFailedOneCannot(t *testing.T) {
	f := newDaoFixture(t, "rejected")
	ctx := context.Background()
	fee := f.treasuryFixture.treasury.Address()
	prepared, err := f.svc.VoteOnChain(ctx, f.scope, f.treasury, f.who,
		&ChainProposal{Proposal: treasuryabi.Proposal{ID: 3, Action: payTo(t, f, keypair.MustRandom().Address())}}, true)
	if err != nil {
		t.Fatalf("VoteOnChain: %v", err)
	}
	view, _ := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash)
	if !strings.Contains(view.Call.Says, "Vote for proposal 3") {
		t.Errorf("says %q", view.Call.Says)
	}

	// Signed by somebody who is not the member: the network says no.
	f.chain.sendStatus = stellarcore.TXStatusError
	wrong := signEnvelope(t, view.Call.XDR, keypair.MustRandom())
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, wrong, fee, f.signFeeBump); !errors.Is(err, settlement.ErrRejected) {
		t.Fatalf("a call the network turned away: %v", err)
	}
	if _, err := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash); err != nil {
		t.Fatalf("it should still be there to sign properly: %v", err)
	}

	// Signed properly, it lands — and the contract refuses it there.
	f.chain.sendStatus, f.chain.failed = stellarcore.TXStatusPending, true
	right := signEnvelope(t, view.Call.XDR, f.wallet)
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, right, fee, f.signFeeBump); !errors.Is(err, ErrCallFailedOnChain) {
		t.Fatalf("a call that failed on chain: %v", err)
	}
	f.chain.failed = false
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, right, fee, f.signFeeBump); !errors.Is(err, store.ErrCallSubmitted) {
		t.Errorf("its sequence number is spent, and it was offered again: %v", err)
	}
	call, err := f.store.MemberCall(ctx, f.scope.Org, f.scope.OwnerRef, prepared.Hash)
	if err != nil || !call.Failed || call.LandedHash == "" {
		t.Errorf("recorded as %+v (%v)", call, err)
	}
}

// Creating a treasury sends one transaction, from the relay, and records what
// came of it as a treasury held by a contract.
func TestCreatingATreasuryRecordsWhatTheNetworkMade(t *testing.T) {
	f := newDaoFixture(t, "create")
	ctx := context.Background()
	voter := keypair.MustRandom().Address()
	policy := treasuryabi.Policy{
		QuorumBps: 5_000, ApprovalBps: 6_000, VotingPeriod: 600, Timelock: 60, SpendLimit: big.NewInt(0),
	}
	encodedPolicy, _ := policy.ScVal()
	members, _ := treasuryabi.Addresses([]string{voter})
	none, _ := treasuryabi.Addresses(nil)
	f.chain.returns = map[string]xdr.ScVal{
		"policy": encodedPolicy, "members": members, "allowlist": none,
		"proposal_count": treasuryabi.U32(0), "spendable": mustAmount(t, 0),
		"member_weight": treasuryabi.U32(1),
	}

	state, err := f.svc.CreateContractTreasury(ctx, f.scope, CreateTreasuryParams{
		Label: "grants", Policy: policy,
		Members: []treasuryabi.Member{{Address: voter, Weight: 1}}, By: f.identity,
	})
	if err != nil {
		t.Fatalf("CreateContractTreasury: %v", err)
	}
	if state.Treasury.Address != createdAt || state.Treasury.Kind != store.TreasuryContract || state.Treasury.Label != "grants" {
		t.Errorf("recorded %+v", state.Treasury)
	}
	if state.TotalWeight != 1 || len(state.Members) != 1 || state.Members[0].Address != voter {
		t.Errorf("read back %+v", state.Members)
	}
	if state.Policy.Timelock != 60 {
		t.Errorf("policy read back %+v", state.Policy)
	}

	// One creation, sent as itself and signed by the relay: nobody's fee bump.
	if f.chain.creations != 1 || len(f.chain.sent) != 1 {
		t.Fatalf("%d creation(s), %d sent", f.chain.creations, len(f.chain.sent))
	}
	parsed, _ := txnbuild.TransactionFromXDR(f.chain.sent[0])
	tx, plain := parsed.Transaction()
	if !plain || tx.SourceAccount().AccountID != f.relay.Address() || len(tx.Signatures()) != 1 {
		t.Errorf("the creation was not sent by the relay, signed once")
	}

	// A constructor that refuses is the contract's refusal, and nothing is
	// recorded or sent.
	f.chain.refuses["__constructor"] = uint32(treasuryabi.InvalidPolicy)
	_, err = f.svc.CreateContractTreasury(ctx, f.scope, CreateTreasuryParams{
		Label: "bad", Policy: policy, Members: []treasuryabi.Member{{Address: voter, Weight: 1}},
	})
	var refusal *ContractRefusal
	if !errors.As(err, &refusal) || refusal.Code != treasuryabi.InvalidPolicy {
		t.Errorf("a refused constructor: %v", err)
	}
	if len(f.chain.sent) != 1 {
		t.Errorf("a refused creation was sent")
	}
	if _, err := f.svc.CreateContractTreasury(ctx, f.scope, CreateTreasuryParams{Label: "empty", Policy: policy}); err == nil {
		t.Error("created a treasury with nobody in it")
	}
}

func mustAmount(t *testing.T, n int64) xdr.ScVal {
	t.Helper()
	v, err := treasuryabi.Amount(big.NewInt(n))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Adopting a contract is two questions put to the network: is it this code,
// and does whoever is asking have a vote in it.
func TestAdoptingAContractAsksWhatItRunsAndWhoVotes(t *testing.T) {
	f := newDaoFixture(t, "adopt")
	ctx := context.Background()
	address := anotherContract()
	f.chain.returns["version"] = treasuryabi.U32(treasuryabi.Version)
	f.chain.returns["member_weight"] = treasuryabi.U32(0)

	// Some other code at that address: none of what is said about a treasury
	// is true of it.
	f.chain.wasm = strings.Repeat("0b", 32)
	if _, err := f.svc.AdoptContractTreasury(ctx, f.scope, address, "x", f.who, f.identity); !errors.Is(err, ErrNotTreasuryCode) {
		t.Fatalf("other code: %v", err)
	}
	f.chain.wasm = treasuryCode

	// The right code, answering an older shape.
	f.chain.returns["version"] = treasuryabi.U32(1)
	if _, err := f.svc.AdoptContractTreasury(ctx, f.scope, address, "x", f.who, f.identity); !errors.Is(err, ErrNotTreasuryCode) {
		t.Fatalf("another version: %v", err)
	}
	f.chain.returns["version"] = treasuryabi.U32(treasuryabi.Version)

	// The right code, and no vote in it.
	_, err := f.svc.AdoptContractTreasury(ctx, f.scope, address, "x", f.who, f.identity)
	var refusal *ContractRefusal
	if !errors.As(err, &refusal) || refusal.Code != treasuryabi.NotMember {
		t.Fatalf("no vote: %v", err)
	}
	if _, err := f.store.TreasuryByAddress(ctx, f.scope.Org, address); !errors.Is(err, store.ErrNoTreasury) {
		t.Fatalf("it was recorded anyway: %v", err)
	}

	// Somebody with no wallet has nothing to be a voter with.
	if _, err := f.svc.AdoptContractTreasury(ctx, f.scope, address, "x", store.Member{}, f.identity); !errors.Is(err, ErrNoWallet) {
		t.Errorf("no wallet: %v", err)
	}
}

// Carrying out a proposal needs nobody's signature, so the relay sends it —
// as itself, with no member's envelope inside.
func TestExecuteIsSentByTheRelay(t *testing.T) {
	f := newDaoFixture(t, "execute")
	ctx := context.Background()

	if _, err := f.svc.ExecuteOnChain(ctx, f.scope, f.treasury, 3); err != nil {
		t.Fatalf("ExecuteOnChain: %v", err)
	}
	parsed, _ := txnbuild.TransactionFromXDR(f.chain.sent[0])
	tx, plain := parsed.Transaction()
	if !plain || tx.SourceAccount().AccountID != f.relay.Address() {
		t.Fatal("execute was not sent by the relay as its own transaction")
	}

	f.chain.refuses["execute"] = uint32(treasuryabi.NotExecutable)
	_, err := f.svc.ExecuteOnChain(ctx, f.scope, f.treasury, 3)
	var refusal *ContractRefusal
	if !errors.As(err, &refusal) || refusal.Code != treasuryabi.NotExecutable {
		t.Errorf("a proposal still in its timelock: %v", err)
	}
	if len(f.chain.sent) != 1 {
		t.Error("an execute the contract would refuse was sent anyway")
	}

	// Settling reports what the contract recorded.
	f.chain.landedWith = ptrTo(statusVal(treasuryabi.Rejected))
	status, err := f.svc.SettleOnChain(ctx, f.scope, f.treasury, 4)
	if err != nil || status != treasuryabi.Rejected {
		t.Errorf("settled as %q, %v", status, err)
	}
}

func statusVal(name string) xdr.ScVal {
	sym := xdr.ScSymbol(name)
	items := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}}
	p := &items
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

// A proposal's note is shown only when it is the note the proposal commits
// to. Prose that does not hash to what is on chain is about something else.
func TestANoteIsShownOnlyWhenItMatchesItsCommitment(t *testing.T) {
	f := newDaoFixture(t, "notes")
	ctx := context.Background()
	fee := f.treasuryFixture.treasury.Address()
	action := payTo(t, f, keypair.MustRandom().Address())

	prepared, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, action, "March invoice")
	if err != nil {
		t.Fatal(err)
	}
	view, _ := f.svc.LoadMemberCall(ctx, f.scope, prepared.Hash)
	f.chain.landedWith = ptrTo(treasuryabi.U32(1))
	if _, err := f.svc.SubmitMemberCall(ctx, f.scope, signEnvelope(t, view.Call.XDR, f.wallet), fee, f.signFeeBump); err != nil {
		t.Fatalf("submit: %v", err)
	}

	onChain := func(memo [32]byte) xdr.ScVal {
		encoded, _ := action.ScVal()
		who, _ := treasuryabi.Address(f.wallet.Address())
		sym := func(s string) xdr.ScVal {
			v := xdr.ScSymbol(s)
			return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
		}
		u64 := func(n uint64) xdr.ScVal { v := xdr.Uint64(n); return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v} }
		entries := xdr.ScMap{
			{Key: sym("action"), Val: encoded},
			{Key: sym("closes_at"), Val: u64(uint64(time.Now().Add(time.Hour).Unix()))},
			{Key: sym("executable_at"), Val: u64(uint64(time.Now().Add(2 * time.Hour).Unix()))},
			{Key: sym("id"), Val: treasuryabi.U32(1)},
			{Key: sym("memo"), Val: treasuryabi.Memo(memo)},
			{Key: sym("no"), Val: treasuryabi.U32(0)},
			{Key: sym("proposer"), Val: who},
			{Key: sym("status"), Val: statusVal(treasuryabi.Open)},
			{Key: sym("yes"), Val: treasuryabi.U32(1)},
		}
		p := &entries
		return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &p}
	}
	f.chain.returns["proposal_count"] = treasuryabi.U32(1)
	f.chain.refuses["tally"] = uint32(treasuryabi.Undecided)

	f.chain.returns["proposal"] = onChain(noteMemo("March invoice"))
	list, err := f.svc.ChainProposals(ctx, f.scope, f.treasury, 5)
	if err != nil || len(list) != 1 {
		t.Fatalf("ChainProposals: %v (%d)", err, len(list))
	}
	if list[0].Note != "March invoice" || list[0].Settles != "" || list[0].Yes != 1 {
		t.Errorf("read %+v", list[0])
	}

	// The contract holds a different commitment under that number: whatever
	// this deployment has on file, it is not this proposal's note.
	f.chain.returns["proposal"] = onChain(noteMemo("something else entirely"))
	one, err := f.svc.ChainProposalByID(ctx, f.scope, f.treasury, 1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Note != "" {
		t.Errorf("a note that does not match its commitment was shown: %q", one.Note)
	}

	// And what settling would record is the contract's answer, not arithmetic
	// done here.
	delete(f.chain.refuses, "tally")
	f.chain.returns["tally"] = statusVal(treasuryabi.Passed)
	one, err = f.svc.ChainProposalByID(ctx, f.scope, f.treasury, 1)
	if err != nil || one.Settles != treasuryabi.Passed {
		t.Errorf("settles %q, %v", one.Settles, err)
	}
}

// Without the configuration, or for a treasury of the other kind, nothing here
// builds anything.
func TestContractTreasuriesAreOffUntilConfigured(t *testing.T) {
	f := newDaoFixture(t, "off")
	ctx := context.Background()
	action := payTo(t, f, keypair.MustRandom().Address())

	// A classic treasury is not a contract, whatever it is asked.
	if _, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasuryRow, f.who, action, ""); !errors.Is(err, ErrNotContractTreasury) {
		t.Errorf("a classic treasury: %v", err)
	}
	// Another workspace's treasury is not this one's to call.
	other := f.treasury
	other.Org = orgFor(t, "off elsewhere").ID
	if _, err := f.svc.ProposeOnChain(ctx, f.scope, other, f.who, action, ""); !errors.Is(err, ErrNotContractTreasury) {
		t.Errorf("another workspace's treasury: %v", err)
	}
	if _, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, store.Member{}, action, ""); !errors.Is(err, ErrNoWallet) {
		t.Errorf("a member with no wallet: %v", err)
	}

	f.svc.cfg.ReadOnly = true
	if _, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, action, ""); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: %v", err)
	}
	f.svc.cfg.ReadOnly = false

	f.svc.cfg.Treasuries = nil
	if _, err := f.svc.ProposeOnChain(ctx, f.scope, f.treasury, f.who, action, ""); !errors.Is(err, ErrContractTreasuriesOff) {
		t.Errorf("unconfigured: %v", err)
	}
	if _, err := f.svc.ExecuteOnChain(ctx, f.scope, f.treasury, 1); !errors.Is(err, ErrContractTreasuriesOff) {
		t.Errorf("unconfigured execute: %v", err)
	}
	if len(f.chain.simulated) != 0 || len(f.chain.sent) != 0 {
		t.Errorf("something reached the network: %v", f.chain.simulated)
	}
	_ = chat.Telegram
}

// The commitment to a note is computed in two places: here, and in the page
// that checks a note against the call it is shown beside. The same two values
// are pinned in web/static/treasury.test.js.
func TestANotesCommitmentIsTheOneThePageComputes(t *testing.T) {
	for note, want := range map[string]string{
		"March invoice": "41fc38841e5f22812dcac1848a468b9ab1bd9c51c62712091f3b746d02a8488b",
		"":              "b40d4ebb204d39ea6ddcddfb44424ae37c16fa644ed545d8ba6140d3dea49085",
	} {
		memo := noteMemo(note)
		if got := hex.EncodeToString(memo[:]); got != want {
			t.Errorf("noteMemo(%q) = %s, want %s", note, got, want)
		}
	}
}

func TestProposalsAndPoliciesAreSaidInWords(t *testing.T) {
	f := newDaoFixture(t, "words")
	to := keypair.MustRandom().Address()
	other := anotherContract()

	for want, action := range map[string]treasuryabi.Action{
		"pay 250.00 XLM to " + to:      payTo(t, f, to),
		"allow payments to " + to:      {Kind: treasuryabi.AllowRecipient, Recipient: to},
		"stop payments to " + to:       {Kind: treasuryabi.DenyRecipient, Recipient: to},
		"stop proposal 7":              {Kind: treasuryabi.Revoke, Target: 7},
		"remove " + to + " as a voter": {Kind: treasuryabi.SetMember, Member: &treasuryabi.Member{Address: to}},
		"give " + to + " a vote of weight 3": {
			Kind: treasuryabi.SetMember, Member: &treasuryabi.Member{Address: to, Weight: 3},
		},
		// A token this deployment cannot account for is named by its address
		// and nothing friendlier.
		"pay 1.00 token " + other + " to " + to: {Kind: treasuryabi.Pay, Payment: &treasuryabi.Payment{
			Token: other, To: to, Amount: big.NewInt(10_000_000),
		}},
	} {
		if got := f.svc.DescribeAction(action); got != want {
			t.Errorf("described as %q, want %q", got, want)
		}
	}

	policy := treasuryabi.Policy{
		QuorumBps: 5_000, ApprovalBps: 6_667, VotingPeriod: 259_200, Timelock: 90_000,
		SpendLimit: big.NewInt(500_000_000), SpendWindow: 86_400,
	}
	want := "quorum 50%, approval 66.67%, voting open 3d, timelock 1d 1h, up to 50.00 without a proposal every 1d"
	if got := DescribePolicy(policy); got != want {
		t.Errorf("policy described as\n  %s\nwant\n  %s", got, want)
	}
	policy.SpendLimit, policy.Timelock = big.NewInt(0), 0
	if got := DescribePolicy(policy); !strings.Contains(got, "timelock none, no spending without a proposal") {
		t.Errorf("policy described as %s", got)
	}
}
