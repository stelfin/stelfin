// Package config loads stelfin's configuration from the environment.
//
// Every secret is required and has no default. A payments server that starts
// with a placeholder signing key, an empty webhook secret, or a development
// treasury is worse than one that refuses to start: the failure is silent, and
// the first sign of it is money in the wrong place. Missing values are
// collected and reported together so a fresh deployment is fixed in one pass
// rather than one restart per variable.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// Config is the whole runtime configuration.
type Config struct {
	// HTTPAddr is the listen address, e.g. ":8080".
	HTTPAddr string
	// BaseURL is the public origin the confirmation page is served from.
	BaseURL string
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string

	// NetworkPassphrase selects testnet or mainnet.
	NetworkPassphrase string
	// HorizonURL is the Horizon instance to use.
	HorizonURL string

	// TreasurySeed is the treasury's secret seed. See the warning on Load.
	TreasurySeed string

	// AssetCode and AssetIssuer identify the asset users transact in.
	AssetCode   string
	AssetIssuer string

	// MetaAppSecret verifies webhook signatures.
	MetaAppSecret []byte
	// MetaVerifyToken answers the subscription challenge.
	MetaVerifyToken string
	// MetaAccessToken authenticates outbound messages.
	MetaAccessToken string
	// MetaPhoneNumberID is the sending number's id.
	MetaPhoneNumberID string

	// ConfirmTokenSecret signs confirmation links. At least 32 bytes.
	ConfirmTokenSecret []byte

	// AnthropicAPIKey authenticates the decoder. Empty lets the SDK resolve
	// credentials itself, which is the right default when running under a
	// configured profile.
	AnthropicAPIKey string
	// DecoderEffort tunes how hard the model works. Empty uses the API default.
	DecoderEffort string

	// ShutdownGrace bounds how long in-flight work has to finish.
	ShutdownGrace time.Duration
}

// Load reads configuration from the environment.
//
// TreasurySeed is read from an environment variable, which is the weakest
// acceptable option and only appropriate for testnet. On mainnet the treasury's
// signing material belongs in an HSM or KMS and should never be materialised in
// the process — the server takes a signing *function*, so swapping this for a
// remote signer is a wiring change, not a redesign.
func Load() (*Config, error) {
	var missing []string
	req := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	opt := func(key, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	c := &Config{
		HTTPAddr:           opt("STELFIN_HTTP_ADDR", ":8080"),
		BaseURL:            req("STELFIN_BASE_URL"),
		DatabaseURL:        req("STELFIN_DATABASE_URL"),
		HorizonURL:         opt("STELFIN_HORIZON_URL", "https://horizon-testnet.stellar.org"),
		TreasurySeed:       req("STELFIN_TREASURY_SEED"),
		AssetCode:          opt("STELFIN_ASSET_CODE", "USDC"),
		AssetIssuer:        req("STELFIN_ASSET_ISSUER"),
		MetaAppSecret:      []byte(req("STELFIN_META_APP_SECRET")),
		MetaVerifyToken:    req("STELFIN_META_VERIFY_TOKEN"),
		MetaAccessToken:    req("STELFIN_META_ACCESS_TOKEN"),
		MetaPhoneNumberID:  req("STELFIN_META_PHONE_NUMBER_ID"),
		ConfirmTokenSecret: []byte(req("STELFIN_CONFIRM_TOKEN_SECRET")),
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		DecoderEffort:      os.Getenv("STELFIN_DECODER_EFFORT"),
	}

	switch strings.ToLower(opt("STELFIN_NETWORK", "testnet")) {
	case "testnet":
		c.NetworkPassphrase = network.TestNetworkPassphrase
	case "public", "mainnet":
		c.NetworkPassphrase = network.PublicNetworkPassphrase
	default:
		return nil, errors.New("config: STELFIN_NETWORK must be testnet or public")
	}

	grace, err := time.ParseDuration(opt("STELFIN_SHUTDOWN_GRACE", "20s"))
	if err != nil {
		return nil, fmt.Errorf("config: STELFIN_SHUTDOWN_GRACE: %w", err)
	}
	c.ShutdownGrace = grace

	if len(missing) > 0 {
		// All of them at once: a deployment is fixed in one pass rather than
		// one restart per variable.
		return nil, fmt.Errorf("config: missing required variables: %s", strings.Join(missing, ", "))
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if len(c.ConfirmTokenSecret) < 32 {
		return fmt.Errorf(
			"config: STELFIN_CONFIRM_TOKEN_SECRET is %d bytes, want at least 32 — "+
				"this signs the links that authorise payments", len(c.ConfirmTokenSecret))
	}
	if len(c.MetaAppSecret) < 16 {
		return fmt.Errorf("config: STELFIN_META_APP_SECRET is implausibly short (%d bytes)",
			len(c.MetaAppSecret))
	}
	if !strkey.IsValidEd25519SecretSeed(c.TreasurySeed) {
		return errors.New("config: STELFIN_TREASURY_SEED is not a valid Stellar secret seed")
	}
	if !strkey.IsValidEd25519PublicKey(c.AssetIssuer) {
		return errors.New("config: STELFIN_ASSET_ISSUER is not a valid Stellar address")
	}
	if !strings.HasPrefix(c.BaseURL, "https://") && !strings.HasPrefix(c.BaseURL, "http://localhost") {
		return errors.New(
			"config: STELFIN_BASE_URL must be https — confirmation links carry payment authority")
	}
	switch c.DecoderEffort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("config: STELFIN_DECODER_EFFORT %q is not a known effort level", c.DecoderEffort)
	}
	return nil
}

// IsMainnet reports whether this configuration points at the public network.
func (c *Config) IsMainnet() bool {
	return c.NetworkPassphrase == network.PublicNetworkPassphrase
}

// Redacted returns the configuration with secrets removed, for logging at
// startup. Every field here is one a reader needs to confirm the process came
// up pointed at what they intended.
func (c *Config) Redacted() map[string]any {
	network := "testnet"
	if c.IsMainnet() {
		network = "public"
	}
	return map[string]any{
		"http_addr":      c.HTTPAddr,
		"base_url":       c.BaseURL,
		"network":        network,
		"horizon_url":    c.HorizonURL,
		"asset":          c.AssetCode + ":" + c.AssetIssuer,
		"phone_id":       c.MetaPhoneNumberID,
		"decoder_effort": c.DecoderEffort,
		"shutdown_grace": c.ShutdownGrace.String(),
		// Secrets are named, never valued, so a startup log confirms they were
		// supplied without disclosing them.
		"secrets_present": "treasury seed, app secret, access token, confirm secret",
	}
}
