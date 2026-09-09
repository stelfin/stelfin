package contracts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestManifestNamesBothContracts(t *testing.T) {
	got := Names()
	if len(got) != 2 || got[0] != ConnectorRegistry || got[1] != DAOTreasury {
		t.Fatalf("Names() = %v", got)
	}
}

func TestLookup(t *testing.T) {
	for _, name := range Names() {
		deployment, err := Lookup(name, Testnet)
		if err != nil {
			t.Fatalf("%s on testnet: %v", name, err)
		}
		if deployment.WasmHash == "" || deployment.UploadTx == "" {
			t.Errorf("%s: %+v", name, deployment)
		}
		if pkg, err := PackageOf(name); err != nil || pkg == "" {
			t.Errorf("%s: package %q, %v", name, pkg, err)
		}
	}

	// Mainnet is an ordinary state, not a failure: a contract exists on testnet
	// long before it exists on mainnet, and a deployment pointed at the second
	// should say so plainly.
	if _, err := Lookup(DAOTreasury, Public); !errors.Is(err, ErrNotDeployed) {
		t.Errorf("mainnet lookup: %v, want ErrNotDeployed", err)
	}
	if _, err := Lookup("no_such_contract", Testnet); err == nil {
		t.Error("looked up a contract that does not exist")
	}
}

// TestTheCommittedHashesAreThisSourcesHashes rebuilds the contracts and
// compares.
//
// Hermetic — no network — and it is the half of the guarantee that lives in
// this repository. The integration test next door proves the hash is on chain;
// this proves the hash is what this source produces. Either alone is a claim
// about something else: an on-chain hash with no matching source is a contract
// nobody here can read, and a source with no matching upload is a contract
// nobody is running.
//
// Skipped when the toolchain is absent, because requiring cargo to run `go
// test ./...` would make the Go suite un-runnable for anyone touching the Go.
func TestTheCommittedHashesAreThisSourcesHashes(t *testing.T) {
	if testing.Short() {
		t.Skip("rebuilding the contracts takes minutes")
	}
	if _, err := exec.LookPath("stellar"); err != nil {
		t.Skip("the stellar CLI is not installed")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not installed")
	}

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve directory: %v", err)
	}

	build := exec.Command("stellar", "contract", "build")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build contracts: %v\n%s", err, out)
	}

	for _, name := range Names() {
		pkg, err := PackageOf(name)
		if err != nil {
			t.Fatalf("package for %s: %v", name, err)
		}
		// Cargo turns a hyphen into an underscore in the artefact name.
		artefact := filepath.Join(root, "target", "wasm32v1-none", "release",
			underscored(pkg)+".wasm")
		if _, err := os.Stat(artefact); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		hash := exec.Command("sha256sum", artefact)
		out, err := hash.Output()
		if err != nil {
			t.Fatalf("hash %s: %v", artefact, err)
		}
		got := string(out[:64])

		deployment, err := Lookup(name, Testnet)
		if err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		if got != deployment.WasmHash {
			t.Errorf(
				"%s: this source builds to %s, and the manifest says %s.\n"+
					"Either the contract changed and the manifest was not updated, or it "+
					"was updated without re-uploading — and the second is worse.",
				name, got, deployment.WasmHash)
		}
	}
}

func underscored(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b == '-' {
			out[i] = '_'
		}
	}
	return string(out)
}
