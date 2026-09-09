//go:build integration

package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// The test that cannot pass unless the upload actually happened.
//
// Everything else about a deployment is a claim this repository makes about
// itself: a hash in a JSON file, a transaction id in a commit message, a link
// in a README. All of it can be written by hand. What cannot be written by hand
// is a contract-code entry in the testnet ledger — so this reads one, hashes
// the WASM the network hands back, and requires it to be the hash committed
// here.
//
// Run with: go test -tags=integration ./contracts/
const testnetRPC = "https://soroban-testnet.stellar.org"

func TestTheCommittedHashesAreOnChain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rpc := rpcclient.NewClient(testnetRPC, &http.Client{Timeout: 30 * time.Second})
	defer rpc.Close()

	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			deployment, err := Lookup(name, Testnet)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}

			raw, err := hex.DecodeString(deployment.WasmHash)
			if err != nil {
				t.Fatalf("decode hash: %v", err)
			}
			var hash xdr.Hash
			copy(hash[:], raw)

			key, err := xdr.MarshalBase64(xdr.LedgerKey{
				Type:         xdr.LedgerEntryTypeContractCode,
				ContractCode: &xdr.LedgerKeyContractCode{Hash: hash},
			})
			if err != nil {
				t.Fatalf("encode ledger key: %v", err)
			}

			resp, err := rpc.GetLedgerEntries(ctx,
				protocol.GetLedgerEntriesRequest{Keys: []string{key}})
			if err != nil {
				t.Fatalf("read ledger entries: %v", err)
			}
			if len(resp.Entries) == 0 {
				t.Fatalf(
					"the network has no contract code under %s.\n"+
						"deployments.json names an upload (%s) that testnet does not have — "+
						"either it never happened, or testnet has been reset since.",
					deployment.WasmHash, deployment.UploadTx)
			}

			var entry xdr.LedgerEntryData
			if err := xdr.SafeUnmarshalBase64(resp.Entries[0].DataXDR, &entry); err != nil {
				t.Fatalf("decode ledger entry: %v", err)
			}
			code, ok := entry.GetContractCode()
			if !ok {
				t.Fatalf("the entry at %s is not contract code", deployment.WasmHash)
			}

			// Hashed rather than compared by id. The network stores WASM under
			// its own hash, so an id that matches proves only that we asked the
			// right question; hashing what came back proves the bytes are the
			// ones this repository is about.
			onChain := sha256.Sum256(code.Code)
			if got := hex.EncodeToString(onChain[:]); got != deployment.WasmHash {
				t.Fatalf("the network returned code hashing to %s under the id %s",
					got, deployment.WasmHash)
			}
		})
	}
}
