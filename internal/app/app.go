// Package app wires stelfin together and runs it: the chat-platform webhooks,
// the confirmation API, and the ingestion workers.
//
// It is a package rather than the body of cmd/stelfind so that the server a
// developer runs locally is the same one that is deployed. cmd/stelfin-dev
// supplies a database and calls Run; it does not carry a second copy of the
// wiring that could drift from this one.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/core"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ingestion"
	"github.com/stelfin/stelfin/internal/config"
	"github.com/stelfin/stelfin/internal/discord"
	"github.com/stelfin/stelfin/internal/money"
	"github.com/stelfin/stelfin/internal/telegram"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/mcp"
	"github.com/stelfin/stelfin/settlement"
	"github.com/stelfin/stelfin/web"
)

// Run loads configuration from the environment and serves until the process is
// signalled or the HTTP server fails.
func Run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("configuration loaded", "config", cfg.Redacted())

	if cfg.ReadOnly {
		log.Warn("STELFIN_READONLY is set: nothing will be built or submitted until it is cleared")
	}
	if cfg.IsMainnet() {
		// The treasury seed is read from the environment, which is fine for
		// testnet and not fine for real money. Say so loudly rather than
		// letting it pass unremarked.
		log.Warn("running against the public network with a treasury seed from the environment; " +
			"move signing into an HSM or KMS before handling real funds")
	}

	// Signals first, so a Ctrl-C during slow startup still stops the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Migrations run before anything opens a pool, so a schema change is
	// applied exactly once by whichever instance starts first.
	if err := ledger.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	db := store.New(pool)
	assetID, err := db.Ledger().EnsureAsset(ctx, cfg.AssetCode, cfg.AssetIssuer)
	if err != nil {
		return err
	}

	// The deployment's own org, holding the float it sponsors reserves and pays
	// fees from. Created here rather than in a migration because the network is
	// configuration, and a network-specific row baked into a migration is how a
	// testnet deployment ends up claiming to be mainnet.
	platform, err := db.EnsurePlatformOrg(ctx, networkName(cfg))
	if err != nil {
		return err
	}
	log.Info("platform org ready", "org", platform.ID, "network", platform.Network)

	// The platform org is one row. Every workspace is bound to a network too,
	// and all of them have to be bound to this one.
	if strays, err := db.OrgsOffNetwork(ctx, networkName(cfg)); err != nil {
		return err
	} else if strays > 0 {
		return fmt.Errorf("%w: %d of them, and this deployment is configured for %s",
			errOrgsOffNetwork, strays, networkName(cfg))
	}

	horizonClient := &horizonclient.Client{
		HorizonURL: cfg.HorizonURL, HTTP: &http.Client{Timeout: 30 * time.Second},
	}
	upstreams := []endpoint{{"STELFIN_HORIZON_URL", func() (string, error) {
		root, err := horizonClient.Root()
		return root.NetworkPassphrase, err
	}}}
	if cfg.HasSoroban() {
		rpc := rpcclient.NewClient(cfg.SorobanRPCURL, &http.Client{Timeout: 30 * time.Second})
		upstreams = append(upstreams, endpoint{"STELFIN_SOROBAN_RPC_URL", func() (string, error) {
			got, err := rpc.GetNetwork(ctx)
			return got.Passphrase, err
		}})
	}
	if err := bindNetwork(log, cfg.NetworkPassphrase, cfg.IsMainnet(), upstreams...); err != nil {
		return err
	}

	treasury, err := keypair.ParseFull(cfg.TreasurySeed)
	if err != nil {
		return fmt.Errorf("parse treasury seed: %w", err)
	}

	settle, err := settlement.New(settlement.Config{
		HorizonURL:        cfg.HorizonURL,
		NetworkPassphrase: cfg.NetworkPassphrase,
	})
	if err != nil {
		return err
	}
	// Optional, and a deployment without it serves everything else and refuses
	// contract calls with a message that says why. Failing to start over a
	// capability nobody has asked for yet would be worse.
	if cfg.HasSoroban() {
		settle, err = settle.DialSoroban(cfg.SorobanRPCURL, nil)
		if err != nil {
			return fmt.Errorf("dial soroban rpc: %w", err)
		}
		log.Info("soroban rpc configured", "url", cfg.SorobanRPCURL)
	}

	// A deployment with no transports configured is legitimate — the signing
	// pages are still served and /webhook/{channel} answers 404 for everything.
	// The registry is built either way, because it carries the rule that a
	// reply containing an authority link may not be posted where bystanders can
	// read it, and that rule should not depend on which platforms are enabled.
	var enabled []chat.Transport
	if cfg.HasTelegram() {
		tg, err := telegram.New(telegram.Config{
			Token:         cfg.TelegramBotToken,
			WebhookSecret: cfg.TelegramWebhookSecret,
			APIURL:        cfg.TelegramAPIURL,
			WebhookURL:    telegramWebhookURL(cfg.BaseURL),
		})
		if err != nil {
			return err
		}
		enabled = append(enabled, tg)
	}
	if cfg.HasDiscord() {
		dc, err := discord.New(discord.Config{
			PublicKey:     cfg.DiscordPublicKey,
			BotToken:      cfg.DiscordBotToken,
			ApplicationID: cfg.DiscordApplicationID,
			APIURL:        cfg.DiscordAPIURL,
		})
		if err != nil {
			return err
		}
		enabled = append(enabled, dc)
	}
	transports, err := chat.NewRegistry(cfg.BaseURL, enabled...)
	if err != nil {
		return err
	}
	log.Info("chat transports registered", "channels", transports.Channels())
	if len(enabled) == 0 {
		log.Warn("no chat transport is configured; the webhook route will refuse every delivery")
	}

	// SEP-10 challenges, if a web-auth key is configured. Its absence is a
	// missing capability rather than a missing dependency: the bot runs, and
	// says linking is unavailable when someone asks for it.
	var challenges *identity.Challenges
	if cfg.HasWebAuth() {
		host := baseHost(cfg.BaseURL)
		challenges, err = identity.New(identity.Config{
			Seed:              cfg.WebAuthSeed,
			HomeDomain:        host,
			WebAuthDomain:     host,
			NetworkPassphrase: cfg.NetworkPassphrase,
		})
		if err != nil {
			return err
		}
		log.Info("address linking enabled", "web_auth_account", challenges.ServerAccount())
	} else {
		log.Warn("no web-auth key configured; members cannot link a wallet")
	}

	connectors, err := connectorsFor(cfg, log)
	if err != nil {
		return err
	}
	reader, err := decoderFor(cfg, log)
	if err != nil {
		return err
	}
	if !cfg.ContractCalls {
		log.Info("contract calls are off; the contract commands are not offered")
	}

	svc, err := api.NewService(pool,
		reader,
		intent.NewResolver(pool),
		settle,
		api.Config{
			Asset:      txnbuild.CreditAsset{Code: cfg.AssetCode, Issuer: cfg.AssetIssuer},
			AssetCode:  cfg.AssetCode,
			AssetID:    int16(assetID),
			Challenges: challenges,

			SponsorAddress: treasury.Address(),

			ReadOnly:      cfg.ReadOnly,
			ReadOnlyFile:  cfg.ReadOnlyFile,
			MaxPayment:    ceiling(cfg.MaxPayment),
			MaxDaily:      ceiling(cfg.MaxDaily),
			Connectors:    connectors,
			ContractCalls: cfg.ContractCalls,
		})
	if err != nil {
		return err
	}

	tokens, err := api.NewConfirmTokens(cfg.ConfirmTokenSecret)
	if err != nil {
		return err
	}
	// Deliberately keyed with the same secret as ConfirmTokens: the version
	// each signs into its MAC is what keeps an enroll token from ever
	// verifying as a confirm token or vice versa, so a second secret would add
	// deployment friction without adding safety.
	enrollTokens, err := api.NewEnrollTokens(cfg.ConfirmTokenSecret)
	if err != nil {
		return err
	}
	linkTokens, err := api.NewLinkTokens(cfg.ConfirmTokenSecret)
	if err != nil {
		return err
	}
	approveTokens, err := api.NewApproveTokens(cfg.ConfirmTokenSecret)
	if err != nil {
		return err
	}

	router, err := core.New(core.Config{
		Store:   db,
		Sender:  svc,
		Admins:  transports,
		Network: networkName(cfg),
		// What this deployment was not given, so that the commands needing it
		// are not offered. A command that can only answer "not here" is a dead
		// button.
		Unavailable: core.Unavailable{
			Model:         cfg.DecoderKind() == config.DecoderGrammar,
			Connectors:    connectors == nil,
			ContractCalls: !cfg.ContractCalls || !cfg.HasSoroban(),
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	server, err := api.NewServer(svc, tokens, enrollTokens, linkTokens, api.ServerConfig{
		BaseURL:         cfg.BaseURL,
		Handler:         router,
		Transports:      transports,
		TreasuryAddress: treasury.Address(),
		SignFeeBump: func(tx *txnbuild.FeeBumpTransaction) (*txnbuild.FeeBumpTransaction, error) {
			return tx.Sign(cfg.NetworkPassphrase, treasury)
		},
		SignProvision: func(tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
			return tx.Sign(cfg.NetworkPassphrase, treasury)
		},
		Approvals:         approveTokens,
		NetworkPassphrase: cfg.NetworkPassphrase,
		Assets:            web.Handler(),
		Logger:            log,
	})
	if err != nil {
		return err
	}

	// stelfin as an MCP server, for agents outside. Read-only tools, reached
	// with a token a workspace admin mints in chat and can revoke there; no
	// token here can sign or submit, because that is not a thing the token
	// table can express.
	agents, err := mcp.NewServer(db, db, log)
	if err != nil {
		return err
	}
	// The propose tier: a token minted for it can draft a payroll for a member
	// to review, and nothing more.
	agents = agents.WithDrafter(svc).WithHoldings(svc)
	routes := http.NewServeMux()
	routes.Handle("POST /mcp", agents.Handler())
	routes.Handle("/", server.Routes())

	ingester, err := ingestion.New(db, pool, ingestion.Config{Logger: log})
	if err != nil {
		return err
	}
	// Named "operations" rather than "payments", and the rename matters: the
	// cursor row is per stream, so a deployment upgrading from the payments
	// endpoint starts this one from the beginning of history rather than
	// resuming mid-stream against another endpoint's paging tokens.
	classic, err := ingestion.NewOperationsSource(
		&horizonclient.Client{HorizonURL: cfg.HorizonURL}, "operations")
	if err != nil {
		return err
	}

	// Ingestion runs alongside the server. Each source owns a durable cursor,
	// so a restart resumes rather than replaying or skipping.
	//
	// One goroutine per source, independent on purpose: a source being
	// unreachable must not stop the others recording, and one falling behind
	// should not hold the rest back.
	sources := []ingestion.Source{classic}
	// Contract-held treasuries move money through token events, not Horizon
	// operations, so a deployment with no RPC configured simply cannot see
	// them. Said out loud at startup rather than left as a silent zero
	// balance for whoever runs one.
	if cfg.HasSoroban() {
		if cfg.SorobanStartLedger == 0 {
			// Not fatal, because it only matters the first time: a deployment
			// that has ingested before resumes from its cursor. But on a fresh
			// one, token-event ingestion will refuse to begin, and it should be
			// said here rather than discovered from a balance that never moves.
			log.Warn("no Soroban start ledger is configured; on a deployment with no " +
				"ingestion cursor yet, token events will not be read until " +
				"STELFIN_SOROBAN_START_LEDGER names where to begin")
		}
		transfers, err := ingestion.NewTransferSource(
			rpcclient.NewClient(cfg.SorobanRPCURL, &http.Client{Timeout: 30 * time.Second}),
			"transfers", cfg.SorobanStartLedger)
		if err != nil {
			return err
		}
		sources = append(sources, transfers)
	} else {
		log.Warn("no Soroban RPC configured; a contract-held treasury's balance " +
			"cannot be seen and will read as zero")
	}
	ingestDone := make(chan struct{})
	var ingesting sync.WaitGroup
	for _, src := range sources {
		ingesting.Add(1)
		go func(src ingestion.Source) {
			defer ingesting.Done()
			if err := ingester.Run(ctx, src); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("ingestion stopped", "source", src.Name(), "error", err)
			}
		}(src)
	}
	// Beside ingestion, and stopped with it: closing proposals whose signing
	// window has passed.
	ingesting.Add(1)
	go func() {
		defer ingesting.Done()
		sweepProposals(ctx, db.ExpireProposals, proposalSweepInterval, log)
	}()
	// And telling each chat platform what it needs telling: where to deliver,
	// and what the commands are. Idempotent, so it happens on every start
	// rather than being a deploy step someone has to remember, and beside the
	// server rather than before it, so that somebody else's API being slow
	// does not hold up this one's.
	ingesting.Add(1)
	go func() {
		defer ingesting.Done()
		prepareTransports(ctx, transports, router.Commands(), prepareBackoffMin, log)
	}()
	go func() {
		ingesting.Wait()
		close(ingestDone)
	}()

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: routes,
		// Bounds on a public endpoint: without them a slow client can hold a
		// connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutting down", "grace", cfg.ShutdownGrace)
	}

	// Stop accepting first, then give in-flight work its grace period. A
	// webhook delivery already acknowledged may still be preparing a payment.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}

	select {
	case <-ingestDone:
	case <-shutdownCtx.Done():
		log.Warn("ingestion did not stop within the grace period")
	}

	log.Info("stopped")
	return nil
}

// ceiling parses a configured ceiling. Config has already refused anything
// that is not a positive amount, so the only way to zero here is unset.
func ceiling(raw string) money.Stroops {
	if raw == "" {
		return 0
	}
	n, err := money.Parse(raw)
	if err != nil {
		return 0
	}
	return n
}

// networkName renders the configured network the way the schema names it.
func networkName(cfg *config.Config) string {
	if cfg.IsMainnet() {
		return "public"
	}
	return "testnet"
}

// baseHost is the host part of the deployment's base URL.
//
// SEP-10 shows a signer the domain they are authenticating to, and checks it
// again on the way back, so it has to be this deployment's own host rather than
// anything configurable independently — a mismatch between the two would let a
// challenge issued here be presented somewhere else.
func baseHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		// config.Load has already required an https base URL, so this is
		// unreachable; returning the raw value keeps the failure visible in the
		// challenge rather than turning it into an empty domain.
		return baseURL
	}
	return u.Host
}
