package settlement

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
)

// How long a contract's entries have left, and paying for more.
//
// Everything a contract is — its code, its instance, each thing it stores — is
// a ledger entry with a lifetime, paid for by the byte. When one runs out the
// entry is archived: it is not gone, but nothing can use it until somebody
// pays to bring it back, and until then every call that touches it fails.
//
// A contract can renew its own entries as it is used, and dao_treasury does,
// for what it owns. It deliberately does not renew its code, which is large
// and shared: see contracts/dao-treasury/src/policy.rs. So that is done here,
// from outside, with the operation the network has for it — which anybody may
// send for any entry, needs no function on the contract, and can be sized.
//
// Sized is the point. An extension costs in proportion to how far it reaches,
// and the reach is chosen by the sender. That is what lets a caller put a
// ceiling on what one extension may cost, where a contract renewing itself to
// a fixed horizon costs whatever the distance to that horizon happens to be.

// Life is one ledger entry's lifetime as the network last reported it.
type Life struct {
	// Found is false for an entry the RPC did not return: one that was never
	// written, or one that has been archived. From here the two look the same.
	Found bool
	// Remaining is how many ledgers the entry has left, as of the ledger the
	// RPC answered at. Zero for one that has just run out.
	Remaining uint32
}

// ContractCodeKey is the ledger key of uploaded code, by its hash.
func ContractCodeKey(wasmHash string) (xdr.LedgerKey, error) {
	raw, err := hex.DecodeString(wasmHash)
	if err != nil || len(raw) != 32 {
		return xdr.LedgerKey{}, fmt.Errorf("settlement: %q is not a code hash", wasmHash)
	}
	var hash xdr.Hash
	copy(hash[:], raw)
	return xdr.LedgerKey{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.LedgerKeyContractCode{Hash: hash},
	}, nil
}

// ContractInstanceKey is the ledger key of a contract's instance: the entry
// that says what it runs and holds its instance storage.
func ContractInstanceKey(contract string) (xdr.LedgerKey, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		return xdr.LedgerKey{}, fmt.Errorf("settlement: %q is not a contract address", contract)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}, nil
}

// maxKeysPerRead is how many entries the RPC answers for in one request.
const maxKeysPerRead = 200

// Lives reports how long each entry has left, in the order asked.
func (c *Client) Lives(ctx context.Context, keys []xdr.LedgerKey) ([]Life, error) {
	if c.soroban == nil {
		return nil, ErrNoSoroban
	}
	reader, ok := c.soroban.(ledgerReader)
	if !ok {
		return nil, fmt.Errorf("%w: this RPC client cannot read ledger entries", ErrNoSoroban)
	}

	out := make([]Life, len(keys))
	for start := 0; start < len(keys); start += maxKeysPerRead {
		end := min(start+maxKeysPerRead, len(keys))
		encoded := make([]string, 0, end-start)
		// By its encoding, because the RPC answers in whatever order it likes
		// and leaves out what it does not have.
		at := make(map[string][]int, end-start)
		for i := start; i < end; i++ {
			key, err := xdr.MarshalBase64(keys[i])
			if err != nil {
				return nil, fmt.Errorf("settlement: encode ledger key: %w", err)
			}
			if _, seen := at[key]; !seen {
				encoded = append(encoded, key)
			}
			at[key] = append(at[key], i)
		}

		resp, err := reader.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: encoded})
		if err != nil {
			return nil, fmt.Errorf("settlement: read ledger entries: %w", err)
		}
		for _, entry := range resp.Entries {
			if entry.LiveUntilLedgerSeq == nil {
				continue
			}
			var remaining uint32
			if until := *entry.LiveUntilLedgerSeq; until > resp.LatestLedger {
				remaining = until - resp.LatestLedger
			}
			for _, i := range at[entry.KeyXDR] {
				out[i] = Life{Found: true, Remaining: remaining}
			}
		}
	}
	return out, nil
}

// PreparedExtension is a transaction that extends some entries' lifetimes,
// ready to be signed, and what it bids to do so.
type PreparedExtension struct {
	Transaction *txnbuild.Transaction
	// ResourceFee is the rent, which is nearly all of what an extension costs.
	ResourceFee money.Stroops
}

// PrepareExtension builds the transaction that makes each of keys live at
// least extendTo ledgers from now, and refuses if it would bid more than
// ceiling for the rent.
//
// The ceiling is the caller's and not the package's general one. Rent is the
// one fee here whose size the sender chooses, by choosing how far to extend —
// so the useful limit is whatever that sender has decided one extension is
// worth, and a caller refused by it can ask for less.
func (c *Client) PrepareExtension(
	ctx context.Context, source string, keys []xdr.LedgerKey, extendTo uint32, ceiling money.Stroops,
) (*PreparedExtension, error) {
	if c.soroban == nil {
		return nil, ErrNoSoroban
	}
	if len(keys) == 0 || extendTo == 0 {
		return nil, errors.New("settlement: nothing to extend")
	}

	extend := &txnbuild.ExtendFootprintTtl{
		ExtendTo: extendTo,
		Ext: xdr.TransactionExt{V: 1, SorobanData: &xdr.SorobanTransactionData{
			Resources: xdr.SorobanResources{Footprint: xdr.LedgerFootprint{ReadOnly: keys}},
		}},
	}
	built, err := c.Build(ctx, BuildRequest{
		Source: source, Operations: []txnbuild.Operation{extend}, Timeout: SorobanTimeout,
	})
	if err != nil {
		return nil, err
	}
	sim, err := c.Simulate(ctx, built)
	if err != nil {
		return nil, err
	}
	if sim.MinResourceFee > ceiling {
		return nil, fmt.Errorf("%w: extending by that much bids %s, and the limit for one extension is %s",
			ErrResourceFeeTooHigh, sim.MinResourceFee, ceiling)
	}

	var data xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionData, &data); err != nil {
		return nil, fmt.Errorf("settlement: decode extension footprint: %w", err)
	}
	if fee := money.Stroops(data.ResourceFee); fee > ceiling {
		return nil, fmt.Errorf("%w: the footprint bids %s", ErrResourceFeeTooHigh, fee)
	}
	extend.Ext = xdr.TransactionExt{V: 1, SorobanData: &data}

	// The same sequence number the simulation ran against: see Assemble.
	account := built.SourceAccount()
	assembled, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &account,
		IncrementSequenceNum: false,
		BaseFee:              c.baseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: built.Timebounds()},
		Operations:           []txnbuild.Operation{extend},
	})
	if err != nil {
		return nil, fmt.Errorf("settlement: assemble extension: %w", err)
	}
	return &PreparedExtension{Transaction: assembled, ResourceFee: sim.MinResourceFee}, nil
}
