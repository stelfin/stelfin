// Command stelfind runs the stelfin server: the WhatsApp webhook, the
// confirmation API, and the Horizon ingestion worker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/stelfin/stelfin/ingestion"
	"github.com/stelfin/stelfin/internal/config"
	"github.com/stelfin/stelfin/internal/whatsapp"
	"github.com/stelfin/stelfin/ledger"
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

	store := ledger.New(pool)
	assetID, err := store.EnsureAsset(ctx, cfg.AssetCode, cfg.AssetIssuer)
	if err != nil {
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

	messenger, err := whatsapp.New(whatsapp.Config{
		PhoneNumberID: cfg.MetaPhoneNumberID,
		AccessToken:   cfg.MetaAccessToken,
	})
	if err != nil {
		return err
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
			Asset:     txnbuild.CreditAsset{Code: cfg.AssetCode, Issuer: cfg.AssetIssuer},
			AssetCode: cfg.AssetCode,
			AssetID:   int16(assetID),
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

	server, err := api.NewServer(svc, tokens, enrollTokens, api.ServerConfig{
		BaseURL:         cfg.BaseURL,
		Messenger:       messenger,
		AppSecret:       cfg.MetaAppSecret,
		VerifyToken:     cfg.MetaVerifyToken,
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
		store, pool, ingestion.Config{Stream: "payments"})
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
