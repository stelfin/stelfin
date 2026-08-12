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
		// Distinctive values. A placeholder like "access" collides with
		// ordinary prose in the redacted output, which makes the leak canary
		// below fire on its own label instead of on a real disclosure.
		"STELFIN_META_APP_SECRET":      "appsecret-7f3a91c4e8b25d60a1f4c9e2b8d7a350",
		"STELFIN_META_VERIFY_TOKEN":    "verifytoken-2c5e81f0",
		"STELFIN_META_ACCESS_TOKEN":    "accesstoken-9b1d4f7e3a06c852",
		"STELFIN_META_PHONE_NUMBER_ID": "123456",
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
		"STELFIN_META_APP_SECRET", "STELFIN_CONFIRM_TOKEN_SECRET",
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
		strings.ToLower(env["STELFIN_META_APP_SECRET"]),
		strings.ToLower(env["STELFIN_META_ACCESS_TOKEN"]),
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
