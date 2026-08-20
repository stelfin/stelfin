package config

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
)

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"STELFIN_BASE_URL":      "https://stelfin.example",
		"STELFIN_DATABASE_URL":  "postgres://localhost/stelfin",
		"STELFIN_TREASURY_SEED": keypair.MustRandom().Seed(),
		"STELFIN_ASSET_ISSUER":  keypair.MustRandom().Address(),
		// A distinctive value. A placeholder like "secret" collides with
		// ordinary prose in the redacted output, which makes the leak canary
		// below fire on its own label instead of on a real disclosure.
		"STELFIN_CONFIRM_TOKEN_SECRET": "confirmsecret-4e9c07a2b6f18d35c0a7e4b93f21d6c8",
	}
}

func withEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoad(t *testing.T) {
	withEnv(t, validEnv(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("http addr = %q, want the default", cfg.HTTPAddr)
	}
	if cfg.IsMainnet() {
		t.Error("defaulted to mainnet; testnet is the safe default")
	}
	if cfg.AssetCode != "USDC" {
		t.Errorf("asset code = %q, want USDC", cfg.AssetCode)
	}
}

// TestLoadReportsEveryMissingVariable: a fresh deployment should be fixable in
// one pass, not one restart per variable.
func TestLoadReportsEveryMissingVariable(t *testing.T) {
	// t.Setenv with empty values clears them for this test only.
	for k := range validEnv(t) {
		t.Setenv(k, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error with no configuration set")
	}
	for _, key := range []string{
		"STELFIN_BASE_URL", "STELFIN_DATABASE_URL", "STELFIN_TREASURY_SEED",
		"STELFIN_ASSET_ISSUER", "STELFIN_CONFIRM_TOKEN_SECRET",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

// TestLoadRejectsWeakSecrets: these sign the links that authorise payments.
func TestLoadRejectsWeakSecrets(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_CONFIRM_TOKEN_SECRET"] = "short"
	withEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("expected a short confirmation secret to be refused")
	}
}

func TestLoadRejectsInvalidStellarMaterial(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"bad seed":   func(e map[string]string) { e["STELFIN_TREASURY_SEED"] = "not-a-seed" },
		"bad issuer": func(e map[string]string) { e["STELFIN_ASSET_ISSUER"] = "not-an-address" },
		// A public key in the seed slot would otherwise pass a naive length check.
		"public key as seed": func(e map[string]string) {
			e["STELFIN_TREASURY_SEED"] = keypair.MustRandom().Address()
		},
	} {
		env := validEnv(t)
		mutate(env)
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestLoadRequiresHTTPS: a confirmation link carries payment authority, so
// sending it over plaintext would hand it to anyone on the path.
func TestLoadRequiresHTTPS(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_BASE_URL"] = "http://stelfin.example"
	withEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("expected a plaintext base url to be refused")
	}

	env["STELFIN_BASE_URL"] = "http://localhost:3000"
	withEnv(t, env)
	if _, err := Load(); err != nil {
		t.Errorf("localhost should be allowed for development: %v", err)
	}
}

func TestLoadRejectsUnknownNetworkAndEffort(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_NETWORK"] = "somethingelse"
	withEnv(t, env)
	if _, err := Load(); err == nil {
		t.Error("expected an unknown network to be refused")
	}

	env = validEnv(t)
	env["STELFIN_DECODER_EFFORT"] = "extreme"
	withEnv(t, env)
	if _, err := Load(); err == nil {
		t.Error("expected an unknown effort level to be refused")
	}
}

// TestRedactedHidesSecrets: this map goes straight into a startup log.
func TestRedactedHidesSecrets(t *testing.T) {
	env := validEnv(t)
	withEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rendered := strings.ToLower(strings.Join(values(cfg.Redacted()), " "))
	for _, secret := range []string{
		strings.ToLower(env["STELFIN_TREASURY_SEED"]),
		strings.ToLower(env["STELFIN_CONFIRM_TOKEN_SECRET"]),
	} {
		if secret == "" {
			t.Fatal("test bug: an empty secret matches everything")
		}
		if strings.Contains(rendered, secret) {
			t.Errorf("redacted config leaks %q:\n%s", secret, rendered)
		}
	}
}

func values(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestTelegramCredentialsAreRequiredTogether: a bot token with no webhook
// secret would leave the delivery endpoint authenticating nothing at all, and a
// secret with no token would configure a transport that cannot reply.
func TestTelegramCredentialsAreRequiredTogether(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"token without secret": {"STELFIN_TELEGRAM_BOT_TOKEN": "12345:ABC"},
		"secret without token": {"STELFIN_TELEGRAM_WEBHOOK_SECRET": strings.Repeat("a", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			full := validEnv(t)
			for k, v := range env {
				full[k] = v
			}
			withEnv(t, full)
			if _, err := Load(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestTelegramWebhookSecretMustBeStrong: Telegram permits a one-character
// secret. With no signature over the body, that secret is the only thing
// standing between the open internet and a delivery this server acts on.
func TestTelegramWebhookSecretMustBeStrong(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_TELEGRAM_BOT_TOKEN"] = "12345:ABC"
	env["STELFIN_TELEGRAM_WEBHOOK_SECRET"] = "hunter2"
	withEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("a seven-character webhook secret was accepted")
	}
}

func TestTelegramIsOptional(t *testing.T) {
	withEnv(t, validEnv(t))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HasTelegram() {
		t.Error("Telegram reported as configured with no credentials set")
	}
}
