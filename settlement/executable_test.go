package settlement

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func instance(exe xdr.ContractExecutable) xdr.LedgerEntryData {
	return xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Val: xdr.ScVal{
				Type:     xdr.ScValTypeScvContractInstance,
				Instance: &xdr.ScContractInstance{Executable: exe},
			},
		},
	}
}

// What is pinned is the code, by its hash, or the fact of being an asset's own
// contract — which has no code that can be swapped.
func TestAContractIsKnownByItsCode(t *testing.T) {
	var hash xdr.Hash
	copy(hash[:], []byte(strings.Repeat("\x01", 32)))
	got, err := executableOf(instance(xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &hash,
	}))
	if err != nil || got.Kind != "wasm" || got.WasmHash != strings.Repeat("01", 32) {
		t.Fatalf("wasm = %+v (%v)", got, err)
	}
	got, err = executableOf(instance(xdr.ContractExecutable{
		Type: xdr.ContractExecutableTypeContractExecutableStellarAsset,
	}))
	if err != nil || got.Kind != "stellar_asset" {
		t.Fatalf("asset contract = %+v (%v)", got, err)
	}
	if _, err := executableOf(xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount}); err == nil {
		t.Fatal("an account entry was read as a contract")
	}
}
