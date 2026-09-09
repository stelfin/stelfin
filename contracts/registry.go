// Package contracts is what stelfin knows about its own on-chain code.
//
// The manifest is embedded rather than read from disk, for the same reason the
// signing pages are: a deployment cannot end up believing in a WASM hash that
// does not match the binary it is running.
//
// The point of the file is provability. A hash written into a repository is a
// claim; a hash the network reports for the same id is a fact. The integration
// test next door compares the two, and it cannot pass unless the upload
// recorded here actually happened — which is the difference between "we
// deployed it" and "we said we deployed it".
package contracts

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

//go:embed deployments.json
var manifestJSON []byte

// Network names, matching the keys in the manifest and orgs.network.
const (
	Testnet = "testnet"
	Public  = "public"
)

// Contract names, as they appear in the manifest.
const (
	DAOTreasury       = "dao_treasury"
	ConnectorRegistry = "connector_registry"
)

// ErrNotDeployed reports a contract that has not been uploaded to a network.
//
// Its own error because it is an ordinary state, not a failure: a contract
// exists on testnet long before it exists on mainnet, and a deployment pointed
// at the second should say so plainly rather than behave as though the file
// were corrupt.
var ErrNotDeployed = errors.New("contracts: not deployed to this network")

// Deployment is one upload, on one network.
type Deployment struct {
	// WasmHash is the sha256 of the built artefact, which is also the id the
	// network stores it under.
	WasmHash string `json:"wasm_hash"`
	// UploadTx is the transaction that put it there, so the claim can be
	// followed back to a ledger by anyone.
	UploadTx   string `json:"upload_tx"`
	UploadedAt string `json:"uploaded_at"`
}

type contractEntry struct {
	Package  string                `json:"package"`
	Networks map[string]Deployment `json:"networks"`
}

type manifest struct {
	Contracts map[string]contractEntry `json:"contracts"`
}

var loaded manifest

func init() {
	if err := json.Unmarshal(manifestJSON, &loaded); err != nil {
		// A malformed manifest is a build-time mistake: the file is embedded,
		// so this cannot become true at runtime on a binary that started.
		panic("contracts: deployments.json is malformed: " + err.Error())
	}
	for name, entry := range loaded.Contracts {
		for network, deployment := range entry.Networks {
			if err := checkHash(deployment.WasmHash); err != nil {
				panic(fmt.Sprintf("contracts: %s on %s: %v", name, network, err))
			}
			if err := checkHash(deployment.UploadTx); err != nil {
				panic(fmt.Sprintf("contracts: %s on %s: upload tx %v", name, network, err))
			}
		}
	}
}

func checkHash(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("%q is %d characters, want 64", s, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("%q is not hex: %w", s, err)
	}
	return nil
}

// Lookup returns what was deployed for a contract on a network.
func Lookup(contract, network string) (Deployment, error) {
	entry, ok := loaded.Contracts[contract]
	if !ok {
		return Deployment{}, fmt.Errorf("contracts: no contract named %q", contract)
	}
	deployment, ok := entry.Networks[network]
	if !ok {
		return Deployment{}, fmt.Errorf("%w: %s on %s", ErrNotDeployed, contract, network)
	}
	return deployment, nil
}

// Names lists every contract in the manifest, in a stable order.
func Names() []string {
	out := make([]string, 0, len(loaded.Contracts))
	for name := range loaded.Contracts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PackageOf reports the Cargo package a contract is built from, which is what
// ties a manifest entry to a directory somebody can rebuild.
func PackageOf(contract string) (string, error) {
	entry, ok := loaded.Contracts[contract]
	if !ok {
		return "", fmt.Errorf("contracts: no contract named %q", contract)
	}
	return entry.Package, nil
}
