// Command stelfin-dev runs stelfin on a developer's machine with nothing
// installed but Go.
//
// The server itself is internal/app, unchanged — this supplies the three things
// a laptop does not have and a deployment does:
//
//   - a database. An embedded Postgres, the same engine the tests use, with its
//     data kept under .dev/ so a restart resumes rather than starting over.
//   - the environment. .env is read here; the server only ever reads the real
//     environment, which is right for a container and tedious for a person.
//   - the throwaway keys. A web-auth key is generated once and kept, and on
//     testnet an operator account that does not exist yet is funded by friendbot.
//
// It refuses the public network outright. Everything it does for convenience —
// a generated key on disk, a seed read from a file — is a thing a deployment
// handling real money must not do.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/internal/app"
)

const (
	stateDir = ".dev"
	// Not 5432: a developer with their own Postgres already has that one.
	pgPort = 54329
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("stelfin-dev stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if err := loadDotEnv(".env"); err != nil {
		return err
	}

	switch strings.ToLower(os.Getenv("STELFIN_NETWORK")) {
	case "", "testnet":
	default:
		return errors.New("stelfin-dev runs against testnet only; use cmd/stelfind for anything else")
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}

	setDefault("STELFIN_BASE_URL", "http://localhost:8080")
	setDefault("STELFIN_SOROBAN_RPC_URL", "https://soroban-testnet.stellar.org")

	for _, key := range []struct{ env, file string }{
		{"STELFIN_CONFIRM_TOKEN_SECRET", "confirm.secret"},
		{"STELFIN_TREASURY_SEED", "operator.seed"},
		{"STELFIN_WEBAUTH_SEED", "webauth.seed"},
	} {
		if os.Getenv(key.env) != "" {
			continue
		}
		seed, created, err := keptSeed(filepath.Join(stateDir, key.file))
		if err != nil {
			return err
		}
		os.Setenv(key.env, seed)
		if created {
			log.Info("generated a development key", "variable", key.env, "kept_in", filepath.Join(stateDir, key.file))
		}
	}

	if os.Getenv("STELFIN_ASSET_ISSUER") == "" {
		// Circle's USDC issuer on testnet. Only a default for this command: the
		// server has none, because the wrong issuer is a different token.
		os.Setenv("STELFIN_ASSET_ISSUER", "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	}

	operator, err := keypair.ParseFull(os.Getenv("STELFIN_TREASURY_SEED"))
	if err != nil {
		return fmt.Errorf("STELFIN_TREASURY_SEED: %w", err)
	}
	if err := ensureFunded(log, operator.Address()); err != nil {
		// Not fatal. The server starts without it, and says so on the first
		// thing that needs the operator to pay.
		log.Warn("could not confirm the operator account is funded", "address", operator.Address(), "error", err)
	}

	if os.Getenv("STELFIN_DATABASE_URL") == "" {
		stop, dsn, err := startPostgres(log)
		if err != nil {
			return err
		}
		defer stop()
		os.Setenv("STELFIN_DATABASE_URL", dsn)
	}

	if os.Getenv("STELFIN_TELEGRAM_BOT_TOKEN") == "" && os.Getenv("STELFIN_DISCORD_PUBLIC_KEY") == "" {
		log.Warn("no chat platform is configured, so nothing can reach the bot; " +
			"set the Telegram or Discord variables in .env — see docs/running-locally.md")
	}

	return app.Run(log)
}

// startPostgres brings up the embedded database and returns its DSN.
func startPostgres(log *slog.Logger) (stop func(), dsn string, err error) {
	root, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", fmt.Errorf("locate home directory: %w", err)
	}

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Username("stelfin").
			Password("stelfin").
			Database("stelfin").
			Port(pgPort).
			// The same cache the tests extract into, so the binary is
			// downloaded once for both.
			BinariesPath(filepath.Join(home, ".embedded-postgres-go", "extracted")).
			// Kept apart from the binaries: the runtime directory is wiped on
			// start, and the data directory is the thing worth keeping.
			RuntimePath(filepath.Join(root, "postgres-runtime")).
			DataPath(filepath.Join(root, "postgres")).
			Logger(nil),
	)
	if err := pg.Start(); err != nil {
		return nil, "", fmt.Errorf("start embedded postgres on port %d: %w", pgPort, err)
	}
	log.Info("embedded postgres ready", "port", pgPort, "data", filepath.Join(stateDir, "postgres"))

	dsn = fmt.Sprintf("postgres://stelfin:stelfin@localhost:%d/stelfin?sslmode=disable", pgPort)
	return func() {
		if err := pg.Stop(); err != nil {
			log.Error("stop embedded postgres", "error", err)
		}
	}, dsn, nil
}

// ensureFunded asks friendbot for an account Horizon has never seen.
func ensureFunded(log *slog.Logger, address string) error {
	horizon := strings.TrimSuffix(os.Getenv("STELFIN_HORIZON_URL"), "/")
	if horizon == "" {
		horizon = "https://horizon-testnet.stellar.org"
	}
	client := &http.Client{Timeout: 45 * time.Second}

	resp, err := client.Get(horizon + "/accounts/" + address)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
	default:
		return fmt.Errorf("horizon answered %s", resp.Status)
	}

	log.Info("operator account does not exist yet; asking friendbot", "address", address)
	resp, err = client.Get("https://friendbot.stellar.org/?addr=" + url.QueryEscape(address))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("friendbot answered %s", resp.Status)
	}
	return nil
}

// keptSeed returns the Stellar seed stored at path, creating one if there is
// none. A random seed is 56 characters of base32, which is also comfortably
// more than the confirm secret's minimum, so one generator serves both.
func keptSeed(path string) (seed string, created bool, err error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(raw)), false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	kp, err := keypair.Random()
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(kp.Seed()+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return kp.Seed(), true, nil
}

func setDefault(key, value string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, value)
	}
}

// loadDotEnv reads KEY=VALUE lines into the environment.
//
// The real environment wins: a variable already set is left alone, so
// `STELFIN_HTTP_ADDR=:9090 make dev` does what it looks like it does. A missing
// file is not an error — every value has a development default.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(text, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: not a KEY=VALUE line", path, line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}
