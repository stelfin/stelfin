package settlement

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// TestEveryCorpusCaseSurvivesBeingStored: a proposal is described again from
// its stored envelope every time somebody opens it, so the description of a
// transaction parsed back from XDR has to be the description of the one that
// was built. A Soroban operation's footprint rides on the transaction rather
// than the operation, which is exactly the kind of thing a round trip drops.
func TestEveryCorpusCaseSurvivesBeingStored(t *testing.T) {
	c := goldenClient(t)
	paths, err := filepath.Glob(filepath.Join(goldenDir(), "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d files)", err, len(paths))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var g goldenCase
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		generic, err := txnbuild.TransactionFromXDR(g.XDR)
		if err != nil {
			t.Fatalf("%s: parse: %v", g.Name, err)
		}
		tx, ok := generic.Transaction()
		if !ok {
			// Fee bumps are described through their own entry point.
			continue
		}
		d, err := c.DescribeTx(tx)
		if err != nil {
			t.Errorf("%s: a stored envelope cannot be described: %v", g.Name, err)
			continue
		}
		if got := d.Canonical(); got != g.Canonical {
			t.Errorf("%s: the stored envelope describes differently:\n%s", g.Name, got)
		}
	}
}
