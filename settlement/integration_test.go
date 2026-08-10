//go:build integration

// Run with: go test -tags=integration ./settlement/
//
// These tests hit the real Stellar testnet through the public Horizon
// instance, so they need network access and take tens of seconds. They are
// tagged out of the default build: `go test ./...` must stay runnable offline.
package settlement

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

const testnetHorizon = "https://horizon-testnet.stellar.org/"

func testnetClient(t *testing.T) (*Client, *horizonclient.Client) {
	t.Helper()
	c, err := New(Config{
		HorizonURL:        testnetHorizon,
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c, &horizonclient.Client{HorizonURL: testnetHorizon}
}

// fundedAccount creates and funds an account via friendbot.
func fundedAccount(t *testing.T, h *horizonclient.Client) *keypair.Full {
	t.Helper()
	kp := keypair.MustRandom()
	if _, err := h.Fund(kp.Address()); err != nil {
		t.Fatalf("friendbot fund %s: %v", kp.Address(), err)
	}
	return kp
}

// TestProvisionOnTestnet is the end-to-end proof of the sponsorship design: a
// user who has never held XLM ends up with a real account carrying a working
// USDC trustline, still holding exactly zero XLM, with the treasury carrying
// every reserve.
func TestProvisionOnTestnet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, h := testnetClient(t)

	treasury := fundedAccount(t, h)
	// A self-created issuer rather than a hardcoded USDC address: the test
	// should not depend on a third party's testnet account still existing.
	issuer := fundedAccount(t, h)

	// The user is never funded. That is the whole point — they have no XLM and
	// no way to get any.
	user := keypair.MustRandom()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer.Address()}
	line, err := usdc.ToChangeTrustAsset()
	if err != nil {
		t.Fatalf("build trustline asset: %v", err)
	}

	tx, err := c.BuildProvision(ctx, treasury.Address(), ProvisionRequest{
		UserAddress: user.Address(),
		Trustlines:  []txnbuild.ChangeTrustAsset{line},
	})
	if err != nil {
		t.Fatalf("build provision: %v", err)
	}

	// Both parties sign: the treasury sponsors and pays, the user accepts.
	signed, err := tx.Sign(network.TestNetworkPassphrase, treasury, user)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	res, err := c.Submit(ctx, signed)
	if err != nil {
		t.Fatalf("submit provisioning transaction: %v", err)
	}
	t.Logf("provisioned in ledger %d, tx %s", res.Ledger, res.Hash)

	acct, err := c.LoadAccount(ctx, user.Address())
	if err != nil {
		t.Fatalf("load provisioned account: %v", err)
	}

	var nativeBalance, trustlineBalance, trustlineSponsor string
	var foundTrustline bool
	for _, b := range acct.Balances {
		if b.Asset.Type == "native" {
			nativeBalance = b.Balance
			continue
		}
		if b.Asset.Code == "USDC" && b.Asset.Issuer == issuer.Address() {
			foundTrustline = true
			trustlineBalance = b.Balance
			trustlineSponsor = b.Sponsor
		}
	}

	if nativeBalance != "0.0000000" {
		t.Errorf("user XLM balance = %q, want \"0.0000000\"; the user must never need XLM", nativeBalance)
	}
	if !foundTrustline {
		t.Fatal("provisioned account has no USDC trustline; it could not receive a cent")
	}
	if trustlineBalance != "0.0000000" {
		t.Errorf("USDC balance = %q, want \"0.0000000\"", trustlineBalance)
	}
	if trustlineSponsor != treasury.Address() {
		t.Errorf("trustline sponsor = %q, want the treasury %s", trustlineSponsor, treasury.Address())
	}
	if acct.NumSponsored == 0 {
		t.Error("account reports no sponsored reserves; the treasury is not carrying them")
	}

	treasuryAcct, err := c.LoadAccount(ctx, treasury.Address())
	if err != nil {
		t.Fatalf("load treasury: %v", err)
	}
	if treasuryAcct.NumSponsoring == 0 {
		t.Error("treasury reports sponsoring nothing")
	}
}

// TestProvisionIsNotUnilateral confirms on a live network that the treasury
// cannot provision an account without the user's signature. If this ever
// passes with one signature, sponsorship has become something done to users
// rather than accepted by them.
func TestProvisionIsNotUnilateral(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, h := testnetClient(t)
	treasury := fundedAccount(t, h)
	issuer := fundedAccount(t, h)
	user := keypair.MustRandom()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer.Address()}
	line, err := usdc.ToChangeTrustAsset()
	if err != nil {
		t.Fatalf("build trustline asset: %v", err)
	}

	tx, err := c.BuildProvision(ctx, treasury.Address(), ProvisionRequest{
		UserAddress: user.Address(),
		Trustlines:  []txnbuild.ChangeTrustAsset{line},
	})
	if err != nil {
		t.Fatalf("build provision: %v", err)
	}

	// Treasury only.
	signed, err := tx.Sign(network.TestNetworkPassphrase, treasury)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := c.Submit(ctx, signed); err == nil {
		t.Fatal("treasury alone provisioned an account; the user's consent is not being required")
	}
}

// TestFeeBumpLetsAZeroBalanceUserTransact proves the second half of the
// design: once provisioned, a user with no XLM can still submit a transaction
// because the treasury pays for it.
func TestFeeBumpLetsAZeroBalanceUserTransact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c, h := testnetClient(t)
	treasury := fundedAccount(t, h)
	issuer := fundedAccount(t, h)
	user := keypair.MustRandom()

	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer.Address()}
	line, err := usdc.ToChangeTrustAsset()
	if err != nil {
		t.Fatalf("build trustline asset: %v", err)
	}

	provision, err := c.BuildProvision(ctx, treasury.Address(), ProvisionRequest{
		UserAddress: user.Address(),
		Trustlines:  []txnbuild.ChangeTrustAsset{line},
	})
	if err != nil {
		t.Fatalf("build provision: %v", err)
	}
	signedProvision, err := provision.Sign(network.TestNetworkPassphrase, treasury, user)
	if err != nil {
		t.Fatalf("sign provision: %v", err)
	}
	if _, err := c.Submit(ctx, signedProvision); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// A user-sourced transaction. The user holds zero XLM, so without a
	// fee-bump this cannot pay its own fee.
	userAcct, err := c.LoadAccount(ctx, user.Address())
	if err != nil {
		t.Fatalf("load user account: %v", err)
	}
	inner, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        userAcct,
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 0}},
		BaseFee:              DefaultBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
	})
	if err != nil {
		t.Fatalf("build inner transaction: %v", err)
	}
	signedInner, err := inner.Sign(network.TestNetworkPassphrase, user)
	if err != nil {
		t.Fatalf("sign inner: %v", err)
	}

	bump, err := c.FeeBump(signedInner, treasury.Address())
	if err != nil {
		t.Fatalf("build fee bump: %v", err)
	}
	signedBump, err := bump.Sign(network.TestNetworkPassphrase, treasury)
	if err != nil {
		t.Fatalf("sign fee bump: %v", err)
	}

	res, err := c.SubmitFeeBump(ctx, signedBump)
	if err != nil {
		t.Fatalf("submit fee-bumped transaction: %v", err)
	}
	t.Logf("fee-bumped transaction landed in ledger %d", res.Ledger)

	after, err := c.LoadAccount(ctx, user.Address())
	if err != nil {
		t.Fatalf("reload user account: %v", err)
	}
	for _, b := range after.Balances {
		if b.Asset.Type == "native" && b.Balance != "0.0000000" {
			t.Errorf("user XLM balance = %q after transacting, want \"0.0000000\"", b.Balance)
		}
	}
}
