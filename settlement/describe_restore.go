package settlement

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// describeRestore renders a RestoreFootprint: which archived contract entries
// it brings back, and what it bids to do so.
//
// What a restore does is in its footprint and nowhere else — the operation
// itself has no fields — so the footprint is the description. Every entry is
// listed. A restore is money spent on somebody's contract state, and the
// signers should see whose and which.
//
// Mirrored by describeRestore in web/static/describe.js, and pinned by the
// golden corpus.
func describeRestore(o *txnbuild.RestoreFootprint) ([]Field, string, error) {
	data := o.Ext.SorobanData
	if data == nil {
		return nil, "", fmt.Errorf("%w: a restore with no footprint", ErrIndescribable)
	}
	if len(data.Resources.Footprint.ReadOnly) != 0 {
		// A restore only writes. A read-only entry in one is not something
		// this knows how to explain, so it is not something to sign.
		return nil, "", fmt.Errorf("%w: a restore with read-only entries", ErrIndescribable)
	}
	entries := data.Resources.Footprint.ReadWrite
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("%w: a restore of nothing", ErrIndescribable)
	}

	fields := []Field{
		{Label: "resource fee", Kind: KindNumber, Value: strconv.FormatInt(int64(data.ResourceFee), 10)},
		{Label: "entries", Kind: KindNumber, Value: strconv.Itoa(len(entries))},
	}
	for i, key := range entries {
		text, err := ledgerKeyString(key)
		if err != nil {
			return nil, "", err
		}
		fields = append(fields, Field{Label: "entry " + strconv.Itoa(i), Kind: KindRaw, Value: text})
	}
	return fields, fmt.Sprintf("Restore %d archived contract entr%s", len(entries), plural(len(entries), "y", "ies")), nil
}

// ledgerKeyString renders a contract ledger key on one line.
func ledgerKeyString(key xdr.LedgerKey) (string, error) {
	switch key.Type {
	case xdr.LedgerEntryTypeContractData:
		cd := key.ContractData
		contract, err := scAddressString(cd.Contract)
		if err != nil {
			return "", err
		}
		value, err := scvalString(cd.Key)
		if err != nil {
			return "", err
		}
		durability := "persistent"
		if cd.Durability == xdr.ContractDataDurabilityTemporary {
			durability = "temporary"
		}
		return "data " + contract + " " + durability + " " + value, nil
	case xdr.LedgerEntryTypeContractCode:
		return "code " + hex.EncodeToString(key.ContractCode.Hash[:]), nil
	default:
		return "", fmt.Errorf("%w: a restore of a %s entry", ErrIndescribable, key.Type)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
