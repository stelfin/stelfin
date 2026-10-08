package app

import (
	"context"
	"log/slog"
	"time"
)

// proposalSweepInterval is how often proposals past their window are closed.
//
// A minute, because the cost is one indexed statement and the alternative is a
// listing that says "open" about something nobody can sign any more.
const proposalSweepInterval = time.Minute

// sweepProposals closes proposals whose signing window has passed: once at
// once, and then every interval until ctx ends.
//
// The first pass is the one that matters on a deployment that sleeps. A
// process that was stopped when a window closed starts up with proposals that
// say "open" and are not, and the first thing it should do is say so.
//
// Nothing depends on this having run. Opening a proposal clears the slot it is
// about to take, and nothing past its window is ever offered for signing. What
// the sweep provides is a record that is true without anybody having asked.
func sweepProposals(
	ctx context.Context, expire func(context.Context) (int64, error),
	every time.Duration, log *slog.Logger,
) {
	pass := func() {
		n, err := expire(ctx)
		switch {
		case ctx.Err() != nil:
			// Shutting down. A statement cut off by that is not a failure.
		case err != nil:
			log.Error("could not close expired proposals", "error", err)
		case n > 0:
			log.Info("closed proposals past their signing window", "count", n)
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
