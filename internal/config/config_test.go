package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

// TestLoadReadsTheHostNotThePrefix: "http://localhost.evil.example" begins with
// "http://localhost" and is somebody else's machine.
func TestLoadReadsTheHostNotThePrefix(t *testing.T) {
	for _, base := range []string{
		"http://localhost.evil.example",
		"http://localhost@evil.example",
		"http://localhostevil.example:8080",
		"ftp://localhost",
		"localhost:8080",
	} {
		env := validEnv(t)
		env["STELFIN_BASE_URL"] = base
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("%s was accepted as a local base url", base)
		}
	}
	for _, base := range []string{"http://localhost", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		env := validEnv(t)
		env["STELFIN_BASE_URL"] = base
		withEnv(t, env)
		if _, err := Load(); err != nil {
			t.Errorf("%s should be allowed for development: %v", base, err)
		}
	}
}

// The bot token is part of every Bot API request path, so an API root over
// plaintext discloses the credential to anyone on the path.
func TestLoadHoldsTheTelegramAPIURLToHTTPS(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_TELEGRAM_API_URL"] = "http://bot-api.example"
	withEnv(t, env)
	if _, err := Load(); err == nil {
		t.Error("a plaintext Telegram API url was accepted")
	}

	for _, ok := range []string{"https://bot-api.example", "http://localhost:8081"} {
		env["STELFIN_TELEGRAM_API_URL"] = ok
		withEnv(t, env)
		cfg, err := Load()
		if err != nil {
			t.Errorf("%s: %v", ok, err)
			continue
		}
		if cfg.TelegramAPIURL != ok {
			t.Errorf("TelegramAPIURL = %q, want %q", cfg.TelegramAPIURL, ok)
		}
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

// TestDiscordCredentialsAreRequiredTogether: a public key with no bot token
// configures a transport that can verify a delivery and never answer it.
func TestDiscordCredentialsAreRequiredTogether(t *testing.T) {
	key := hex.EncodeToString(make([]byte, ed25519.PublicKeySize))

	for name, env := range map[string]map[string]string{
		"key only":   {"STELFIN_DISCORD_PUBLIC_KEY": key},
		"token only": {"STELFIN_DISCORD_BOT_TOKEN": "bot"},
		"key and token, no application": {
			"STELFIN_DISCORD_PUBLIC_KEY": key,
			"STELFIN_DISCORD_BOT_TOKEN":  "bot",
		},
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

// TestDiscordPublicKeyIsCheckedAtBoot: a mistyped key would otherwise produce
// an endpoint that verifies nothing successfully and refuses every delivery,
// which looks like Discord being broken rather than a typo.
func TestDiscordPublicKeyIsCheckedAtBoot(t *testing.T) {
	for _, key := range []string{"not-hex", "aabb", strings.Repeat("a", 62)} {
		env := validEnv(t)
		env["STELFIN_DISCORD_PUBLIC_KEY"] = key
		env["STELFIN_DISCORD_BOT_TOKEN"] = "bot"
		env["STELFIN_DISCORD_APPLICATION_ID"] = "111"
		withEnv(t, env)

		if _, err := Load(); err == nil {
			t.Errorf("accepted public key %q", key)
		}
	}
}

func TestBothTransportsCanBeConfigured(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_TELEGRAM_BOT_TOKEN"] = "12345:ABC"
	env["STELFIN_TELEGRAM_WEBHOOK_SECRET"] = strings.Repeat("a", 32)
	env["STELFIN_DISCORD_PUBLIC_KEY"] = hex.EncodeToString(make([]byte, ed25519.PublicKeySize))
	env["STELFIN_DISCORD_BOT_TOKEN"] = "bot"
	env["STELFIN_DISCORD_APPLICATION_ID"] = "111"
	env["STELFIN_WEBAUTH_SEED"] = keypair.MustRandom().Seed()
	withEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HasTelegram() || !cfg.HasDiscord() {
		t.Errorf("telegram = %v, discord = %v; want both", cfg.HasTelegram(), cfg.HasDiscord())
	}
}

// TestAReachableBotNeedsTheLinkingKey: the key is optional on a deployment
// nobody can speak to. Once somebody can, a bot without it can be spoken to and
// can finish nothing — no wallet linked, no treasury proved, so no payment —
// and that is worse than one that will not start, because only the second gets
// fixed.
func TestAReachableBotNeedsTheLinkingKey(t *testing.T) {
	platforms := map[string]map[string]string{
		"telegram": {
			"STELFIN_TELEGRAM_BOT_TOKEN":      "12345:ABC",
			"STELFIN_TELEGRAM_WEBHOOK_SECRET": strings.Repeat("a", 32),
		},
		"discord": {
			"STELFIN_DISCORD_PUBLIC_KEY":     hex.EncodeToString(make([]byte, ed25519.PublicKeySize)),
			"STELFIN_DISCORD_BOT_TOKEN":      "bot",
			"STELFIN_DISCORD_APPLICATION_ID": "111",
		},
	}
	for name, vars := range platforms {
		t.Run(name, func(t *testing.T) {
			env := validEnv(t)
			delete(env, "STELFIN_WEBAUTH_SEED")
			for k, v := range vars {
				env[k] = v
			}
			withEnv(t, env)

			_, err := Load()
			if err == nil {
				t.Fatal("a bot somebody can reach started with no way to link a wallet")
			}
			if !strings.Contains(err.Error(), "STELFIN_WEBAUTH_SEED") {
				t.Errorf("the refusal does not name the variable to set: %v", err)
			}
		})
	}

	// With no platform configured it stays optional: the signing pages are all
	// there is, and they need no challenge key.
	env := validEnv(t)
	delete(env, "STELFIN_WEBAUTH_SEED")
	withEnv(t, env)
	if _, err := Load(); err != nil {
		t.Fatalf("a deployment with no chat platform was refused for it: %v", err)
	}
}

// TestWebAuthSeedMustNotBeTheTreasury: it signs attacker-chosen material and is
// the one key here that cannot go behind a remote signer. Pointing it at the
// treasury would put the key that pays for everything in that position.
func TestWebAuthSeedMustNotBeTheTreasury(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_WEBAUTH_SEED"] = env["STELFIN_TREASURY_SEED"]
	withEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("the treasury key was accepted as the web-auth key")
	}
}

func TestWebAuthSeedMustBeAStellarSecret(t *testing.T) {
	env := validEnv(t)
	env["STELFIN_WEBAUTH_SEED"] = "not-a-seed"
	withEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("an invalid web-auth seed was accepted")
	}
}

func TestWebAuthIsOptional(t *testing.T) {
	withEnv(t, validEnv(t))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HasWebAuth() {
		t.Error("web auth reported as configured with no seed set")
	}
}

// TestThePublicNetworkHasToBeAskedFor: the operator's key is read from the
// environment, which is acceptable on testnet and is not something a deployment
// should drift into on the public network because one variable was copied from
// somewhere else.
func TestThePublicNetworkHasToBeAskedFor(t *testing.T) {
	mainnet := func(t *testing.T) map[string]string {
		env := validEnv(t)
		env["STELFIN_NETWORK"] = "public"
		env["STELFIN_HORIZON_URL"] = "https://horizon.stellar.org"
		env["STELFIN_MAINNET_ACK"] = MainnetAcknowledgement
		env["STELFIN_MAX_PAYMENT"] = "25000"
		env["STELFIN_MAX_DAILY"] = "100000"
		return env
	}

	withEnv(t, mainnet(t))
	if _, err := Load(); err != nil {
		t.Fatalf("a fully acknowledged public-network configuration was refused: %v", err)
	}

	for name, mutate := range map[string]func(map[string]string){
		"no acknowledgement":        func(e map[string]string) { e["STELFIN_MAINNET_ACK"] = "" },
		"a boolean acknowledgement": func(e map[string]string) { e["STELFIN_MAINNET_ACK"] = "true" },
		"no per-payment ceiling":    func(e map[string]string) { e["STELFIN_MAX_PAYMENT"] = "" },
		"no daily ceiling":          func(e map[string]string) { e["STELFIN_MAX_DAILY"] = "" },
		"the testnet horizon":       func(e map[string]string) { e["STELFIN_HORIZON_URL"] = "" },
	} {
		env := mainnet(t)
		mutate(env)
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("%s: a public-network deployment started anyway", name)
		}
	}
}

// A kill switch that read a typo as "off" would be off on the one day somebody
// was typing in a hurry.
func TestTheReadOnlySwitchIsParsedStrictly(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "ON": true} {
		env := validEnv(t)
		env["STELFIN_READONLY"] = value
		withEnv(t, env)
		cfg, err := Load()
		if err != nil {
			t.Errorf("STELFIN_READONLY=%q: %v", value, err)
			continue
		}
		if cfg.ReadOnly != want {
			t.Errorf("STELFIN_READONLY=%q read as %v", value, cfg.ReadOnly)
		}
	}
	for _, value := range []string{"ture", "enabled", "2"} {
		env := validEnv(t)
		env["STELFIN_READONLY"] = value
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("STELFIN_READONLY=%q was accepted", value)
		}
	}
}

func TestCeilingsHaveToBeAmounts(t *testing.T) {
	for _, value := range []string{"lots", "-5", "0"} {
		env := validEnv(t)
		env["STELFIN_MAX_PAYMENT"] = value
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("STELFIN_MAX_PAYMENT=%q was accepted", value)
		}
	}
}

func TestConnectorSettingsAreCheckedAtBoot(t *testing.T) {
	for _, bad := range []string{"short", strings.Repeat("zz", 32), strings.Repeat("ab", 31)} {
		env := validEnv(t)
		env["STELFIN_CONNECTOR_KEY"] = bad
		withEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("STELFIN_CONNECTOR_KEY=%q was accepted", bad)
		}
	}

	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	env := validEnv(t)
	env["STELFIN_CONNECTOR_KEY"] = strings.Repeat("ab", 32)
	env["STELFIN_GOOGLE_SERVICE_ACCOUNT_FILE"] = path
	withEnv(t, env)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ConnectorKey) != 32 || string(cfg.GoogleServiceAccount) != `{"type":"service_account"}` {
		t.Fatalf("connector settings read as key %d bytes, account %q", len(cfg.ConnectorKey), cfg.GoogleServiceAccount)
	}
	// The log line names that they are set, never what they are.
	if fmt.Sprint(cfg.Redacted()) == "" || strings.Contains(fmt.Sprint(cfg.Redacted()), "service_account") {
		t.Errorf("the redacted configuration carries the key: %v", cfg.Redacted())
	}

	// Both forms at once is ambiguous about which one is meant.
	env["STELFIN_GOOGLE_SERVICE_ACCOUNT"] = `{"type":"service_account"}`
	withEnv(t, env)
	if _, err := Load(); err == nil {
		t.Error("both the inline key and a key file were accepted")
	}
	env["STELFIN_GOOGLE_SERVICE_ACCOUNT"] = ""
	env["STELFIN_GOOGLE_SERVICE_ACCOUNT_FILE"] = filepath.Join(t.TempDir(), "missing.json")
	withEnv(t, env)
	if _, err := Load(); err == nil {
		t.Error("a key file that does not exist was accepted")
	}
}

// TestTheDecoderFollowsTheKeys: with nothing said, the keys say it. And no key
// at all means no model, not "Anthropic, with whatever its SDK can find" — on a
// server that has nothing to find, that was a decoder failing on every message.
func TestTheDecoderFollowsTheKeys(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"nothing":               {nil, DecoderGrammar},
		"an anthropic key":      {map[string]string{"ANTHROPIC_API_KEY": "sk-ant"}, DecoderAnthropic},
		"a compatible key":      {map[string]string{"STELFIN_DECODER_API_KEY": "gsk", "STELFIN_DECODER_MODEL": "m"}, DecoderOpenAI},
		"both keys":             {map[string]string{"ANTHROPIC_API_KEY": "sk-ant", "STELFIN_DECODER_API_KEY": "gsk", "STELFIN_DECODER_MODEL": "m"}, DecoderOpenAI},
		"both, and said":        {map[string]string{"STELFIN_DECODER": "anthropic", "ANTHROPIC_API_KEY": "sk-ant", "STELFIN_DECODER_API_KEY": "gsk"}, DecoderAnthropic},
		"a key, and none asked": {map[string]string{"STELFIN_DECODER": "grammar", "ANTHROPIC_API_KEY": "sk-ant"}, DecoderGrammar},
		"anthropic by name":     {map[string]string{"STELFIN_DECODER": "Anthropic"}, DecoderAnthropic},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv(t)
			for _, k := range []string{"ANTHROPIC_API_KEY", "STELFIN_DECODER", "STELFIN_DECODER_API_KEY", "STELFIN_DECODER_MODEL", "STELFIN_DECODER_BASE_URL"} {
				delete(env, k)
			}
			for k, v := range c.set {
				env[k] = v
			}
			withEnv(t, env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.DecoderKind(); got != c.want {
				t.Errorf("DecoderKind = %q, want %q", got, c.want)
			}
		})
	}
}

// TestACompatibleDecoderIsRefusedHalfConfigured: with a key and no model it
// would start, read nothing, and say so only when somebody wrote a sentence.
func TestACompatibleDecoderIsRefusedHalfConfigured(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"a key and no model":     {"STELFIN_DECODER_API_KEY": "gsk"},
		"asked for, with no key": {"STELFIN_DECODER": "openai", "STELFIN_DECODER_MODEL": "m"},
		"a key sent over http":   {"STELFIN_DECODER_API_KEY": "gsk", "STELFIN_DECODER_MODEL": "m", "STELFIN_DECODER_BASE_URL": "http://api.example.com/v1"},
		"a decoder nobody has":   {"STELFIN_DECODER": "gemini"},
	} {
		t.Run(name, func(t *testing.T) {
			env := validEnv(t)
			for k, v := range set {
				env[k] = v
			}
			withEnv(t, env)
			if _, err := Load(); err == nil {
				t.Fatal("accepted")
			}
		})
	}

	// On this machine http is fine, as it is for the base URL: a local model
	// server is an ordinary thing to run.
	env := validEnv(t)
	env["STELFIN_DECODER_API_KEY"], env["STELFIN_DECODER_MODEL"] = "local", "llama"
	env["STELFIN_DECODER_BASE_URL"] = "http://localhost:11434/v1"
	withEnv(t, env)
	if _, err := Load(); err != nil {
		t.Fatalf("a model server on this machine was refused: %v", err)
	}
}
