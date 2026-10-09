package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/settlement"
)

// Keeping the treasury contract alive.
//
// Everything on a Soroban ledger has a lifetime that is paid for, and what is
// not paid for is archived. For a treasury held by a contract that means two
// things can lapse under it: the code every treasury runs, and each treasury's
// own instance. Either one archived, every vote and every payment fails until
// somebody pays to bring it back.
//
// A treasury renews its own small state whenever it is used. The code it does
// not renew, on purpose — it is large, shared, and nobody's vote should carry
// the bill for it — so a deployment that offers these treasuries has to keep
// the code alive itself, and should not leave an idle treasury to lapse
// either. This is that.
//
// It is deliberately unambitious. It does not keep things alive for months;
// it tops them up a little, often, so that what any one transaction costs is
// small and known, and so that a deployment which stops running stops paying.

const (
	ledgersPerDay = 17_280

	// keepFloor is when to act: less than three days left.
	keepFloor = 3 * ledgersPerDay
	// keepTarget is how far to reach: five days.
	keepTarget = 5 * ledgersPerDay
	// keepLeast is the smallest extension worth a transaction: an hour.
	keepLeast = ledgersPerDay / 24

	// keepCeiling is the most one extension may bid in rent: two XLM.
	//
	// Rent depends on how full the network's state is and is not this
	// deployment's to set. What is its to set is how much it reaches for, so
	// an extension that would cost more than this is asked for again at half
	// the reach, and again, until it fits or is not worth sending.
	keepCeiling = money.Stroops(20_000_000)

	// keepBatch is how many treasuries' instances go in one extension.
	keepBatch = 20
)

// Kept is what became of one attempt to extend something.
type Kept struct {
	// What names it: "the treasury code", or how many treasuries.
	What string
	// Had and Now are the ledgers it had left and has now. Now is zero when
	// nothing was extended.
	Had, Now uint32
	// Cost is the rent bid.
	Cost money.Stroops
	// Problem says why nothing was done, when something should have been.
	Problem string
}

// KeepContractsAlive tops up the lifetime of the treasury code and of every
// treasury's instance that is running low, and reports what it did.
//
// Nothing is reported for what had plenty left, which is the usual case and is
// not news.
func (s *Service) KeepContractsAlive(ctx context.Context) ([]Kept, error) {
	cfg, err := s.contractTreasuriesOn()
	if err != nil {
		return nil, nil
	}
	if err := s.writable(); err != nil {
		return nil, nil
	}

	code, err := settlement.ContractCodeKey(cfg.WasmHash)
	if err != nil {
		return nil, err
	}
	addresses, err := s.store.ContractTreasuryAddresses(ctx)
	if err != nil {
		return nil, err
	}
	keys := []xdr.LedgerKey{code}
	for _, address := range addresses {
		key, err := settlement.ContractInstanceKey(address)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}

	lives, err := s.settle.Lives(ctx, keys)
	if err != nil {
		return nil, err
	}

	var report []Kept

	// The code, on its own: it is most of the cost, and its failing should
	// not hold up anything else.
	switch life := lives[0]; {
	case !life.Found:
		report = append(report, Kept{
			What: "the treasury code",
			Problem: "it is not on the network: never uploaded, or archived. No treasury can be used " +
				"until it is uploaded again (make build-contracts, then stellar contract upload)",
		})
	case life.Remaining < keepFloor:
		report = append(report, s.extend(ctx, cfg, "the treasury code", []xdr.LedgerKey{code}, life.Remaining))
	}

	// Then the treasuries that are running low, a batch at a time.
	var (
		low     []xdr.LedgerKey
		least   uint32
		missing int
	)
	flush := func() {
		if len(low) == 0 {
			return
		}
		what := fmt.Sprintf("%d treasuries", len(low))
		if len(low) == 1 {
			what = "1 treasury"
		}
		report = append(report, s.extend(ctx, cfg, what, low, least))
		low, least = nil, 0
	}
	for i, life := range lives[1:] {
		switch {
		case !life.Found:
			missing++
		case life.Remaining < keepFloor:
			if len(low) == 0 || life.Remaining < least {
				least = life.Remaining
			}
			low = append(low, keys[i+1])
			if len(low) == keepBatch {
				flush()
			}
		}
	}
	flush()
	if missing > 0 {
		report = append(report, Kept{
			What: fmt.Sprintf("%d of %d treasuries", missing, len(addresses)),
			Problem: "archived, or never created. An archived treasury is brought back by the first " +
				"call to it, at that call's expense",
		})
	}
	return report, nil
}

// extend sends one extension, asking for less each time it would cost too
// much, and says what happened.
func (s *Service) extend(
	ctx context.Context, cfg *ContractTreasuryConfig, what string, keys []xdr.LedgerKey, had uint32,
) Kept {
	out := Kept{What: what, Had: had}

	relaying.Lock()
	defer relaying.Unlock()

	target := uint32(keepTarget)
	for {
		prepared, err := s.settle.PrepareExtension(ctx, cfg.RelayAddress, keys, target, keepCeiling)
		if errors.Is(err, settlement.ErrResourceFeeTooHigh) {
			// Half the reach, and so about half the cost.
			reach := (target - had) / 2
			if reach < keepLeast {
				out.Problem = fmt.Sprintf(
					"even an hour more costs over the limit of %s for one extension; rent on this "+
						"network is higher than this deployment will pay unattended", keepCeiling)
				return out
			}
			target = had + reach
			continue
		}
		if err != nil {
			out.Problem = err.Error()
			return out
		}

		signed, err := cfg.SignRelay(prepared.Transaction)
		if err != nil {
			out.Problem = "sign: " + err.Error()
			return out
		}
		res, err := s.settle.SubmitSoroban(ctx, signed)
		switch {
		case err != nil:
			out.Problem = err.Error()
		case res.Failed:
			out.Problem = "the network included the extension and it failed (transaction " + res.Hash + ")"
		default:
			out.Now, out.Cost = target, prepared.ResourceFee
		}
		return out
	}
}
