package settlement

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
)

// What code is behind a contract address.
//
// A Soroban contract can be upgraded: the address stays the same and the code
// behind it is replaced, in one transaction, by whoever the contract lets do
// that. A contract read on Tuesday can be different on Wednesday with no signal
// to anyone calling it. So a workspace does not allow an address; it allows an
// address *running particular code*, and that is checked again before every
// call is built and again before it is submitted.

// ErrUnknownContract reports an address with no contract instance on chain.
var ErrUnknownContract = errors.New("settlement: no contract at that address")

// Executable is what a contract runs.
type Executable struct {
	// Kind is "wasm" for uploaded code, or "stellar_asset" for the built-in
	// contract every classic asset can have, which has no code to change.
	Kind string
	// WasmHash is the hex SHA-256 of the code, for a wasm contract.
	WasmHash string
}

// String is what a signer is shown: what is pinned, in one line.
func (e Executable) String() string {
	if e.Kind == "wasm" {
		return "wasm " + e.WasmHash
	}
	return e.Kind
}

// ledgerReader is the part of an RPC client that reads ledger entries. Kept
// apart from SorobanAPI so the fakes that test submission need not implement it.
type ledgerReader interface {
	GetLedgerEntries(ctx context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error)
}

// ContractExecutable reads, from the network, what a contract runs now.
func (c *Client) ContractExecutable(ctx context.Context, contract string) (Executable, error) {
	if c.soroban == nil {
		return Executable{}, ErrNoSoroban
	}
	reader, ok := c.soroban.(ledgerReader)
	if !ok {
		return Executable{}, fmt.Errorf("%w: this RPC client cannot read ledger entries", ErrNoSoroban)
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		return Executable{}, fmt.Errorf("settlement: %q is not a contract address", contract)
	}
	var id xdr.ContractId
	copy(id[:], raw)

	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	encoded, err := xdr.MarshalBase64(key)
	if err != nil {
		return Executable{}, fmt.Errorf("settlement: encode instance key: %w", err)
	}

	resp, err := reader.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: []string{encoded}})
	if err != nil {
		return Executable{}, fmt.Errorf("settlement: read contract %s: %w", contract, err)
	}
	if len(resp.Entries) == 0 {
		return Executable{}, fmt.Errorf("%w: %s", ErrUnknownContract, contract)
	}

	var data xdr.LedgerEntryData
	if err := xdr.SafeUnmarshalBase64(resp.Entries[0].DataXDR, &data); err != nil {
		return Executable{}, fmt.Errorf("settlement: decode contract instance: %w", err)
	}
	return executableOf(data)
}

func executableOf(data xdr.LedgerEntryData) (Executable, error) {
	if data.ContractData == nil || data.ContractData.Val.Instance == nil {
		return Executable{}, errors.New("settlement: that ledger entry is not a contract instance")
	}
	exe := data.ContractData.Val.Instance.Executable
	switch exe.Type {
	case xdr.ContractExecutableTypeContractExecutableWasm:
		if exe.WasmHash == nil {
			return Executable{}, errors.New("settlement: a wasm contract with no code hash")
		}
		return Executable{Kind: "wasm", WasmHash: hex.EncodeToString(exe.WasmHash[:])}, nil
	case xdr.ContractExecutableTypeContractExecutableStellarAsset:
		return Executable{Kind: "stellar_asset"}, nil
	default:
		return Executable{}, fmt.Errorf("settlement: a contract executable of kind %d", exe.Type)
	}
}
