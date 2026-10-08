// Package config loads stelfin's configuration from the environment.
//
// Every secret is required and has no default. A payments server that starts
// with a placeholder signing key or a development treasury is worse than one
// that refuses to start: the failure is silent, and
// the first sign of it is money in the wrong place. Missing values are
// collected and reported together so a fresh deployment is fixed in one pass
// rather than one restart per variable.
package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stelfin/stelfin/internal/money"
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
	// SorobanRPCURL is the Soroban RPC endpoint.
	//
	// Optional. Without it a deployment serves everything else and refuses
	// contract calls with a message that says why, which is the honest state
	// for one that has not been pointed at an RPC — rather than failing to
	// start over a capability nobody has asked for yet.
	SorobanRPCURL string
	// SorobanStartLedger is where event ingestion begins when it has no cursor.
	//
	// No default, and deliberately so. An RPC keeps a bounded window of events,
	// so "start at the beginning" is not a thing it can serve — and adopting
	// whatever it still holds would silently skip everything before that.
	// Choosing the ledger is how an operator says which history they accept
	// having missed.
	SorobanStartLedger uint32

	// TreasurySeed is the treasury's secret seed. See the warning on Load.
	TreasurySeed string

	// WebAuthSeed signs SEP-10 challenges.
	//
	// Optional: without it a deployment simply cannot link addresses, and says
	// so, rather than refusing to start.
	//
	// It must be a different key from the treasury, and that is checked. It
	// signs arbitrary attacker-chosen challenge material and controls nothing,
	// which is the only reason it is acceptable for it to live in the process
	// at all — txnbuild.BuildChallengeTx takes a seed rather than a signing
	// function, so this is the one key here that cannot go behind a remote
	// signer. Pointing it at the treasury would put the key that pays for
	// everything in exactly that position.
	WebAuthSeed string

	// AssetCode and AssetIssuer identify the asset users transact in.
	AssetCode   string
	AssetIssuer string

	// TelegramBotToken and TelegramWebhookSecret configure the Telegram
	// transport. Optional, and required together: a deployment with neither
	// simply serves no Telegram webhook, which is a legitimate state while the
	// transports are being built out.
	//
	// The secret is held to the same length bar as ConfirmTokenSecret because
	// Telegram does not sign its deliveries — it echoes this value in a header,
	// so it is the only thing authenticating them.
	TelegramBotToken      string
	TelegramWebhookSecret string
	// TelegramAPIURL overrides the Bot API root. Optional; empty means
	// Telegram's own.
	//
	// Telegram publishes its Bot API server for self-hosting, and a deployment
	// that runs one points here. The bot token is part of every request path,
	// so this is held to https unless it is this machine — the same bar as the
	// base URL, for the same reason.
	TelegramAPIURL string

	// DiscordPublicKey, DiscordBotToken and DiscordApplicationID configure the
	// Discord transport. Optional, and required together.
	//
	// Unlike Telegram, the public key is not a secret and not a bearer: it
	// verifies an Ed25519 signature over each delivery, so a leaked copy lets
	// nobody forge one. The bot token is a credential and the application id is
	// public.
	DiscordPublicKey     string
	DiscordBotToken      string
	DiscordApplicationID string

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

	// DiscordAPIURL overrides Discord's REST root. Optional, and for a proxy an
	// operator runs or a test: Discord offers nothing to self-host. Held to
	// https unless it is this machine, because the bot token is sent to it.
	DiscordAPIURL string

	// ReadOnly is the deployment-wide kill switch. Everything that reads keeps
	// working; nothing new is built and nothing is submitted to the network.
	//
	// An environment variable rather than something in the database, on
	// purpose: the moment it is needed is the moment the database, or whatever
	// writes to it, may be the thing that is not trusted.
	ReadOnly bool
	// ReadOnlyFile is the same switch without a restart: while a file exists
	// at this path, nothing is built or submitted. Looked for on every write.
	ReadOnlyFile string

	// MaxPayment and MaxDaily are this deployment's own ceilings on treasury
	// proposals, in whole units of the asset, across every workspace. Empty
	// means none. Required on the public network: a deployment handling real
	// money with no ceiling of its own is relying on every workspace to have
	// set one.
	MaxPayment string
	MaxDaily   string

	// ConnectorKey seals what a workspace's connectors need kept: where each
	// one is and the token it is reached with. Thirty-two bytes, hex. Optional:
	// without it no MCP server or webhook can be registered.
	//
	// Losing it loses every sealed endpoint and credential, and changing it
	// does the same. That is the right failure: they are re-entered on a page
	// by the people who know them, and re-entering one ends its grant.
	ConnectorKey []byte
	// GoogleServiceAccount is a Google service-account key, as the JSON Google
	// issues. Optional: without it there is no Sheets connector. One account
	// for the deployment; a workspace shares a spreadsheet with its address,
	// as a viewer.
	GoogleServiceAccount []byte

	// ContractCalls turns on calling allowed third-party contracts from a
	// treasury. Off unless set: a contract call moves money in ways the spend
	// ceilings cannot see, so a deployment relying on them must opt in.
	ContractCalls bool

	// MainnetAck is the operator saying, in so many words, that this
	// deployment is meant to handle real funds. See validate.
	MainnetAck string
}

// MainnetAcknowledgement is what STELFIN_MAINNET_ACK has to say, exactly.
//
// A sentence and not a boolean. "true" is what a variable gets set to by
// someone copying a block of configuration from a staging environment; nobody
// types this by accident.
const MainnetAcknowledgement = "this deployment moves real funds"

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
	// A ledger sequence that will not parse is a misconfiguration, not a
	// reason to fall back: silently starting from zero would skip history the
	// operator meant to include.
	optLedger := func(key string) uint32 {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return 0
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			missing = append(missing, key+" (must be a ledger sequence)")
			return 0
		}
		return uint32(n)
	}

	c := &Config{
		HTTPAddr:    opt("STELFIN_HTTP_ADDR", ":8080"),
		BaseURL:     req("STELFIN_BASE_URL"),
		DatabaseURL: req("STELFIN_DATABASE_URL"),
		HorizonURL:  opt("STELFIN_HORIZON_URL", defaultTestnetHorizon),
		// No default. A default would point a mainnet deployment at whatever
		// endpoint happened to be written here, and an RPC decides what a
		// contract call is simulated against.
		SorobanRPCURL:      strings.TrimSpace(os.Getenv("STELFIN_SOROBAN_RPC_URL")),
		SorobanStartLedger: optLedger("STELFIN_SOROBAN_START_LEDGER"),
		TreasurySeed:       req("STELFIN_TREASURY_SEED"),
		WebAuthSeed:        strings.TrimSpace(os.Getenv("STELFIN_WEBAUTH_SEED")),
		AssetCode:          opt("STELFIN_ASSET_CODE", "USDC"),
		AssetIssuer:        req("STELFIN_ASSET_ISSUER"),

		TelegramBotToken:      strings.TrimSpace(os.Getenv("STELFIN_TELEGRAM_BOT_TOKEN")),
		TelegramWebhookSecret: strings.TrimSpace(os.Getenv("STELFIN_TELEGRAM_WEBHOOK_SECRET")),
		TelegramAPIURL:        strings.TrimSpace(os.Getenv("STELFIN_TELEGRAM_API_URL")),

		DiscordPublicKey:     strings.TrimSpace(os.Getenv("STELFIN_DISCORD_PUBLIC_KEY")),
		DiscordBotToken:      strings.TrimSpace(os.Getenv("STELFIN_DISCORD_BOT_TOKEN")),
		DiscordApplicationID: strings.TrimSpace(os.Getenv("STELFIN_DISCORD_APPLICATION_ID")),
		ConfirmTokenSecret:   []byte(req("STELFIN_CONFIRM_TOKEN_SECRET")),
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		DecoderEffort:        os.Getenv("STELFIN_DECODER_EFFORT"),

		DiscordAPIURL: strings.TrimSpace(os.Getenv("STELFIN_DISCORD_API_URL")),
		ReadOnlyFile:  strings.TrimSpace(os.Getenv("STELFIN_READONLY_FILE")),
		MaxPayment:    strings.TrimSpace(os.Getenv("STELFIN_MAX_PAYMENT")),
		MaxDaily:      strings.TrimSpace(os.Getenv("STELFIN_MAX_DAILY")),
		MainnetAck:    strings.TrimSpace(os.Getenv("STELFIN_MAINNET_ACK")),
	}

	if raw := strings.TrimSpace(os.Getenv("STELFIN_CONNECTOR_KEY")); raw != "" {
		key, err := hex.DecodeString(raw)
		if err != nil || len(key) != 32 {
			return nil, errors.New(
				"config: STELFIN_CONNECTOR_KEY must be 32 bytes, hex-encoded — openssl rand -hex 32")
		}
		c.ConnectorKey = key
	}
	// The key itself, or a file holding it. A file is what a secret manager
	// mounts; the variable is what a platform's dashboard takes.
	switch inline, path := strings.TrimSpace(os.Getenv("STELFIN_GOOGLE_SERVICE_ACCOUNT")),
		strings.TrimSpace(os.Getenv("STELFIN_GOOGLE_SERVICE_ACCOUNT_FILE")); {
	case inline != "" && path != "":
		return nil, errors.New(
			"config: set STELFIN_GOOGLE_SERVICE_ACCOUNT or STELFIN_GOOGLE_SERVICE_ACCOUNT_FILE, not both")
	case inline != "":
		c.GoogleServiceAccount = []byte(inline)
	case path != "":
		key, err := os.ReadFile(path)
		if err != nil {
			// The path, and nothing of what was or was not in it.
			return nil, fmt.Errorf("config: STELFIN_GOOGLE_SERVICE_ACCOUNT_FILE: cannot read %s", path)
		}
		c.GoogleServiceAccount = key
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("STELFIN_CONTRACT_CALLS"))) {
	case "", "0", "false", "off", "no":
	case "1", "true", "on", "yes":
		c.ContractCalls = true
	default:
		return nil, errors.New("config: STELFIN_CONTRACT_CALLS must be true or false")
	}

	// Parsed strictly. A kill switch that read "ture" as off would be off on
	// the one day somebody was typing in a hurry.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STELFIN_READONLY"))) {
	case "", "0", "false", "off", "no":
	case "1", "true", "on", "yes":
		c.ReadOnly = true
	default:
		return nil, errors.New("config: STELFIN_READONLY must be true or false")
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
	if (c.TelegramBotToken == "") != (c.TelegramWebhookSecret == "") {
		return errors.New(
			"config: STELFIN_TELEGRAM_BOT_TOKEN and STELFIN_TELEGRAM_WEBHOOK_SECRET " +
				"must be set together — a bot with no webhook secret cannot " +
				"authenticate a delivery at all")
	}
	if n := len(c.TelegramWebhookSecret); n > 0 && n < telegramMinSecretLength {
		return fmt.Errorf(
			"config: STELFIN_TELEGRAM_WEBHOOK_SECRET is %d characters, want at least %d — "+
				"Telegram does not sign deliveries, so this secret is the only thing "+
				"authenticating them", n, telegramMinSecretLength)
	}
	if c.TelegramAPIURL != "" && !secureOrLocal(c.TelegramAPIURL) {
		return errors.New(
			"config: STELFIN_TELEGRAM_API_URL must be https — the bot token is " +
				"part of every request sent to it")
	}
	if c.DiscordAPIURL != "" && !secureOrLocal(c.DiscordAPIURL) {
		return errors.New(
			"config: STELFIN_DISCORD_API_URL must be https — the bot token is sent to it")
	}
	for name, value := range map[string]string{
		"STELFIN_MAX_PAYMENT": c.MaxPayment, "STELFIN_MAX_DAILY": c.MaxDaily,
	} {
		if value == "" {
			continue
		}
		if n, err := money.Parse(value); err != nil || n <= 0 {
			return fmt.Errorf("config: %s must be a positive amount, like 25000", name)
		}
	}
	if c.IsMainnet() {
		// The settled decision this implements: an operator key read from the
		// environment does not start against the public network unless the
		// operator has said, deliberately, that it should.
		if c.MainnetAck != MainnetAcknowledgement {
			return fmt.Errorf(
				"config: STELFIN_NETWORK is public, and this deployment's operator key is "+
					"read from the environment. Set STELFIN_MAINNET_ACK to %q to start anyway",
				MainnetAcknowledgement)
		}
		if c.MaxPayment == "" || c.MaxDaily == "" {
			return errors.New(
				"config: on the public network STELFIN_MAX_PAYMENT and STELFIN_MAX_DAILY " +
					"are required — a classic multisig has no contract to enforce a limit, " +
					"so the deployment's own ceilings are the only ones guaranteed to exist")
		}
		if c.HorizonURL == defaultTestnetHorizon {
			return errors.New(
				"config: STELFIN_NETWORK is public and STELFIN_HORIZON_URL is still the " +
					"testnet default; set it to a public-network Horizon")
		}
	}
	discord := []string{c.DiscordPublicKey, c.DiscordBotToken, c.DiscordApplicationID}
	set := 0
	for _, v := range discord {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != len(discord) {
		return errors.New(
			"config: STELFIN_DISCORD_PUBLIC_KEY, STELFIN_DISCORD_BOT_TOKEN and " +
				"STELFIN_DISCORD_APPLICATION_ID must be set together")
	}
	if c.DiscordPublicKey != "" {
		// Checked here rather than only at transport construction, so a
		// mistyped key is a configuration error at boot instead of a silent
		// endpoint that refuses every delivery.
		key, err := hex.DecodeString(c.DiscordPublicKey)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf(
				"config: STELFIN_DISCORD_PUBLIC_KEY must be %d hex-encoded bytes",
				ed25519.PublicKeySize)
		}
	}
	if !strkey.IsValidEd25519SecretSeed(c.TreasurySeed) {
		return errors.New("config: STELFIN_TREASURY_SEED is not a valid Stellar secret seed")
	}
	if c.WebAuthSeed != "" {
		if !strkey.IsValidEd25519SecretSeed(c.WebAuthSeed) {
			return errors.New("config: STELFIN_WEBAUTH_SEED is not a valid Stellar secret seed")
		}
		if c.WebAuthSeed == c.TreasurySeed {
			return errors.New(
				"config: STELFIN_WEBAUTH_SEED must not be the treasury key — it signs " +
					"attacker-chosen material and is the one key here that cannot go " +
					"behind a remote signer")
		}
	}
	if !strkey.IsValidEd25519PublicKey(c.AssetIssuer) {
		return errors.New("config: STELFIN_ASSET_ISSUER is not a valid Stellar address")
	}
	if !secureOrLocal(c.BaseURL) {
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

// defaultTestnetHorizon is where an unconfigured deployment points. Named so
// the public-network check can recognise a deployment that forgot to change it.
const defaultTestnetHorizon = "https://horizon-testnet.stellar.org"

// secureOrLocal reports whether a URL is https, or plain http to this machine.
//
// The host is parsed rather than prefix-matched: "http://localhost.evil.example"
// starts with "http://localhost" and is not this machine.
func secureOrLocal(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// telegramMinSecretLength mirrors telegram.MinSecretLength. Duplicated rather
// than imported so this package keeps depending on nothing but the SDK: config
// is loaded before any transport is constructed, and a validation failure here
// should read as a configuration problem rather than a transport one.
const telegramMinSecretLength = 32

// HasTelegram reports whether the Telegram transport is configured.
func (c *Config) HasTelegram() bool { return c.TelegramBotToken != "" }

// HasDiscord reports whether the Discord transport is configured.
func (c *Config) HasDiscord() bool { return c.DiscordPublicKey != "" }

// HasWebAuth reports whether SEP-10 linking is configured.
func (c *Config) HasWebAuth() bool { return c.WebAuthSeed != "" }

// HasSoroban reports whether contract calls are available on this deployment.
func (c *Config) HasSoroban() bool { return c.SorobanRPCURL != "" }

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
		"soroban_rpc":    c.HasSoroban(),
		"asset":          c.AssetCode + ":" + c.AssetIssuer,
		"telegram":       c.HasTelegram(),
		"discord":        c.HasDiscord(),
		"web_auth":       c.HasWebAuth(),
		"decoder_effort": c.DecoderEffort,
		"read_only":      c.ReadOnly,
		"read_only_file": c.ReadOnlyFile,
		"connector_key":  len(c.ConnectorKey) > 0,
		"contract_calls": c.ContractCalls,
		"google_sheets":  len(c.GoogleServiceAccount) > 0,
		"max_payment":    c.MaxPayment,
		"max_daily":      c.MaxDaily,
		"shutdown_grace": c.ShutdownGrace.String(),
		// Secrets are named, never valued, so a startup log confirms they were
		// supplied without disclosing them.
		"secrets_present": "treasury seed, confirm secret",
	}
}
