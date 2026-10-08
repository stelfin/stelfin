package app

import (
	"errors"
	"fmt"
	"log/slog"
)

// endpoint is one upstream that can say which network it serves.
type endpoint struct {
	// name is how it is referred to in a log line or an error: the variable an
	// operator would go and change.
	name string
	// passphrase asks the endpoint which network it is on.
	passphrase func() (string, error)
}

// bindNetwork checks that every upstream is on the network this deployment was
// configured for.
//
// The passphrase is what a signature is made over. A deployment configured for
// the public network and pointed at a testnet Horizon builds transactions from
// testnet sequence numbers, signs them for mainnet, and submits them to a
// network that rejects every one — and the first anybody hears of it is a
// payment that has collected its signatures and will not land. The reverse
// mistake is quieter and worse: testnet balances read as if they were real.
//
// A mismatch is always fatal. An endpoint that cannot be reached is fatal on
// the public network, where starting blind is not acceptable, and a warning on
// testnet, where a public endpoint having a bad minute should not keep a
// developer's server down.
func bindNetwork(log *slog.Logger, want string, mainnet bool, endpoints ...endpoint) error {
	for _, e := range endpoints {
		got, err := e.passphrase()
		if err != nil {
			if mainnet {
				return fmt.Errorf("app: could not confirm which network %s serves: %w", e.name, err)
			}
			log.Warn("could not confirm which network an endpoint serves; continuing because this is testnet",
				"endpoint", e.name, "error", err)
			continue
		}
		if got == "" {
			return fmt.Errorf("app: %s did not say which network it serves", e.name)
		}
		if got != want {
			return fmt.Errorf(
				"app: %s serves %q and this deployment is configured for %q — "+
					"every transaction built here would be signed for one network "+
					"and sent to the other", e.name, got, want)
		}
	}
	return nil
}

// errOrgsOffNetwork reports books written for the other network.
var errOrgsOffNetwork = errors.New("app: this database holds workspaces from a different network")
