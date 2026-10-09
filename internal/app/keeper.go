package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/stelfin/stelfin/api"
)

// keepAliveInterval is how often the treasury contract's lifetime is looked
// at.
//
// Several times a day, against lifetimes measured in days: what it buys is
// that each top-up is small, and that a deployment which was asleep for one
// pass is not asleep for the margin.
const keepAliveInterval = 6 * time.Hour

// keepContractsAlive tops up the lifetime of the treasury code and of the
// treasuries held by it, at start and then on a timer, until ctx is done.
//
// At start as well, because a deployment that sleeps and is woken is exactly
// the one whose timer cannot be trusted to have fired.
//
// What it spends is logged every time it spends anything. It is the one thing
// this deployment pays for that nobody asked it to do just then, and whoever
// funds the operator account should be able to see it going.
func keepContractsAlive(
	ctx context.Context, keep func(context.Context) ([]api.Kept, error),
	every time.Duration, log *slog.Logger,
) {
	pass := func() {
		report, err := keep(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("could not check how long the treasury contract has left", "error", err)
			return
		}
		for _, k := range report {
			switch {
			case k.Problem != "":
				log.Error("the treasury contract's lifetime was not extended",
					"what", k.What, "ledgers_left", k.Had, "problem", k.Problem)
			default:
				log.Info("extended the treasury contract's lifetime",
					"what", k.What, "ledgers_left_before", k.Had, "ledgers_left_now", k.Now,
					"rent", k.Cost.String())
			}
		}
	}

	pass()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pass()
		}
	}
}
