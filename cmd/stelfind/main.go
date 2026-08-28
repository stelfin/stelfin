// Command stelfind runs the stelfin server: the chat-platform webhooks, the
// confirmation API, and the Horizon ingestion worker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api"
	"github.com/stelfin/stelfin/api/decoder"
	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/core"
	"github.com/stelfin/stelfin/identity"
	"github.com/stelfin/stelfin/ingestion"
	"github.com/stelfin/stelfin/internal/config"
	"github.com/stelfin/stelfin/internal/discord"
	"github.com/stelfin/stelfin/internal/telegram"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
	"github.com/stelfin/stelfin/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("configuration loaded", "config", cfg.Redacted())

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

	svc, err := api.NewService(pool,
		decoder.New(decoder.Config{
			APIKey: cfg.AnthropicAPIKey,
			// Config has already rejected anything that is not a known level.
			Effort: anthropic.OutputConfigEffort(cfg.DecoderEffort),
		}),
		intent.NewResolver(pool),
		settle,
		api.Config{
			Asset:      txnbuild.CreditAsset{Code: cfg.AssetCode, Issuer: cfg.AssetIssuer},
			AssetCode:  cfg.AssetCode,
			AssetID:    int16(assetID),
			Challenges: challenges,
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

	router, err := core.New(core.Config{
		Store:   db,
		Sender:  svc,
		Admins:  transports,
		Network: networkName(cfg),
		Logger:  log,
	})
	if err != nil {
		return err
	}

	// Published to every configured platform. Idempotent, so it runs on each
	// start rather than being a deploy step someone has to remember.
	//
	// A failure here is logged rather than fatal. The command list is a menu
	// affordance — the check that decides what anyone may actually do happens
	// in Go, on every invocation — so a platform being briefly unreachable
	// should not stop a payments server from coming up.
	if err := transports.RegisterCommands(ctx, router.Commands()); err != nil {
		log.Warn("could not publish the command list; the bot still works, "+
			"but autocomplete may be stale", "error", err)
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
		NetworkPassphrase: cfg.NetworkPassphrase,
		Assets:            web.Handler(),
		Logger:            log,
	})
	if err != nil {
		return err
	}

	ingester, err := ingestion.New(ctx,
		&horizonclient.Client{HorizonURL: cfg.HorizonURL},
		db, pool, ingestion.Config{Stream: "payments"})
	if err != nil {
		return err
	}

	// Ingestion runs alongside the server. It owns a durable cursor, so a
	// restart resumes rather than replaying or skipping.
	ingestDone := make(chan struct{})
	go func() {
		defer close(ingestDone)
		if err := ingester.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("ingestion stopped", "error", err)
		}
	}()

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: server.Routes(),
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
