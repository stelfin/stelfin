package app

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

const (
	testnet = "Test SDF Network ; September 2015"
	public  = "Public Global Stellar Network ; September 2015"
)

func says(passphrase string) func() (string, error) {
	return func() (string, error) { return passphrase, nil }
}

func unreachable() (string, error) { return "", errors.New("dial tcp: i/o timeout") }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// TestAnEndpointOnTheOtherNetworkStopsStartup: found at startup, this is one
// line in a log. Found at the first payment, it is a proposal that collected
// its signatures and will never land.
func TestAnEndpointOnTheOtherNetworkStopsStartup(t *testing.T) {
	err := bindNetwork(quiet, public, true,
		endpoint{"STELFIN_HORIZON_URL", says(public)},
		endpoint{"STELFIN_SOROBAN_RPC_URL", says(testnet)})
	if err == nil {
		t.Fatal("a testnet RPC behind a public-network deployment was accepted")
	}
	if !strings.Contains(err.Error(), "STELFIN_SOROBAN_RPC_URL") {
		t.Errorf("the error does not name the variable to change: %v", err)
	}

	if err := bindNetwork(quiet, testnet, false, endpoint{"STELFIN_HORIZON_URL", says(public)}); err == nil {
		t.Error("a public Horizon behind a testnet deployment was accepted")
	}
	if err := bindNetwork(quiet, public, true,
		endpoint{"STELFIN_HORIZON_URL", says(public)},
		endpoint{"STELFIN_SOROBAN_RPC_URL", says(public)}); err != nil {
		t.Errorf("matching endpoints were refused: %v", err)
	}
}

// An endpoint that will not answer is not evidence of anything. On testnet that
// is a warning; on the public network starting blind is not acceptable.
func TestAnUnreachableEndpointIsFatalOnlyWhereItMatters(t *testing.T) {
	if err := bindNetwork(quiet, testnet, false, endpoint{"STELFIN_HORIZON_URL", unreachable}); err != nil {
		t.Errorf("testnet startup was stopped by an unreachable endpoint: %v", err)
	}
	if err := bindNetwork(quiet, public, true, endpoint{"STELFIN_HORIZON_URL", unreachable}); err == nil {
		t.Error("a public-network deployment started without confirming its Horizon")
	}
	if err := bindNetwork(quiet, testnet, false, endpoint{"STELFIN_HORIZON_URL", says("")}); err == nil {
		t.Error("an endpoint that named no network was accepted")
	}
}
