package app

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/contracts"
	"github.com/stelfin/stelfin/internal/config"
)

// contractTreasuriesFor works out whether this deployment can hold a treasury
// in a contract, and with what.
//
// Two things have to be true, and neither is a reason not to start: there is a
// Soroban RPC to reach a contract through, and the treasury code this binary
// was built with has been uploaded to the network it is running on. Without
// either the answer is nil, the commands that need it are not offered, and the
// log says which was missing.
//
// The code's hash comes from the manifest compiled into the binary, not from
// configuration. What a treasury created here runs is a property of the build,
// and a variable that could point it at other code would be a way to hand a
// workspace a treasury that is not the one this source describes.
func contractTreasuriesFor(
	cfg *config.Config, operator *keypair.Full, log *slog.Logger,
) (*api.ContractTreasuryConfig, error) {
	if !cfg.HasSoroban() {
		log.Info("no soroban rpc configured; treasuries held by a contract are not offered")
		return nil, nil
	}
	deployment, err := contracts.Lookup(contracts.DAOTreasury, networkName(cfg))
	if errors.Is(err, contracts.ErrNotDeployed) {
		log.Warn("the treasury contract has not been uploaded to this network; "+
			"treasuries held by a contract are not offered", "network", networkName(cfg))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	relay := operator
	if cfg.RelaySeed != "" {
		relay, err = keypair.ParseFull(cfg.RelaySeed)
		if err != nil {
			return nil, fmt.Errorf("parse relay seed: %w", err)
		}
	}
	log.Info("treasuries held by a contract are available",
		"wasm_hash", deployment.WasmHash, "relay", relay.Address(),
		"relay_is_operator", relay.Address() == operator.Address())

	return &api.ContractTreasuryConfig{
		WasmHash:     deployment.WasmHash,
		RelayAddress: relay.Address(),
		SignRelay: func(tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
			return tx.Sign(cfg.NetworkPassphrase, relay)
		},
	}, nil
}
