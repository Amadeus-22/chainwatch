// Command chainwatch watches wallets for risky ERC-20 approvals and drains.
// main only wires dependencies, starts the workers and shuts them down.
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"

	"github.com/Amadeus-22/chainwatch/internal/adapter/ethrpc"
	"github.com/Amadeus-22/chainwatch/internal/adapter/httpapi"
	"github.com/Amadeus-22/chainwatch/internal/adapter/notify"
	"github.com/Amadeus-22/chainwatch/internal/adapter/postgres"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/platform/config"
	"github.com/Amadeus-22/chainwatch/internal/platform/logging"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

var version = "dev"

// Sign-in timing: a nonce is good for one sign-in within NonceTTL; clocks of the
// browser and the server may differ by clockSkew.
const (
	nonceTTL  = 10 * time.Minute
	clockSkew = time.Minute
)

func main() {
	if err := run(); err != nil {
		slog.Error("chainwatch stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)

	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	checks := []httpapi.Check{{Name: "postgres", Fn: store.Ping}}
	var indexers []*app.Indexer
	var chains []domain.Chain
	var chainIDs []domain.ChainID
	for _, c := range cfg.Chains {
		client := ethrpc.New(c.RPCURL, c.ID, cfg.RPCTimeout, m)
		checks = append(checks, httpapi.Check{Name: fmt.Sprintf("rpc_%d", c.ID), Fn: client.Ping})
		indexers = append(indexers, app.NewIndexer(client, store, app.IndexerConfig{
			ChainID:       c.ID,
			Confirmations: c.Confirmations,
			ReorgWindow:   c.ReorgWindow,
			MaxBlockRange: c.MaxBlockRange,
			StartBlock:    c.StartBlock,
			PollInterval:  c.PollInterval,
			Risk:          risk.Params{LargeOutflow: c.LargeOutflow, DrainWindow: cfg.DrainWindow},
		}, log.With("component", "indexer", "chain_id", uint64(c.ID), "chain", c.Name), m))
		chains = append(chains, domain.Chain{ID: c.ID, Name: c.Name, ExplorerURL: c.ExplorerURL})
		chainIDs = append(chainIDs, c.ID)
		log.Info("chain configured", "chain_id", uint64(c.ID), "chain", c.Name,
			"confirmations", c.Confirmations, "reorg_window", c.ReorgWindow)
	}
	alerter := app.NewAlerter(store, newNotifier(cfg, log), cfg.NotifyInterval, log.With("component", "alerter"), m)
	auth := app.NewAuth(store, store, app.AuthConfig{
		Domain: cfg.SIWEDomain, ChainIDs: chainIDs, NonceTTL: nonceTTL, SessionTTL: cfg.SessionTTL, ClockSkew: clockSkew,
	})
	server := httpapi.NewServer(cfg.HTTPAddr, httpapi.NewHandler(httpapi.Config{
		Auth:         auth,
		Workspace:    app.NewWorkspace(store, chains),
		Checks:       checks,
		Gatherer:     reg,
		Log:          log.With("component", "http"),
		Domain:       cfg.SIWEDomain,
		CookieSecure: cfg.CookieSecure,
		WebDir:       cfg.WebDir,
	}))

	log.Info("chainwatch starting", "version", version, "http_addr", cfg.HTTPAddr,
		"chains", len(cfg.Chains), "siwe_domain", cfg.SIWEDomain)

	g, gctx := errgroup.WithContext(ctx)
	for _, ix := range indexers {
		g.Go(func() error { return ix.Run(gctx) })
	}
	g.Go(func() error { return alerter.Run(gctx) })
	g.Go(func() error {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	})
	return g.Wait()
}

func newNotifier(cfg config.Config, log *slog.Logger) app.Notifier {
	switch {
	case cfg.WebhookURL != "":
		return notify.NewWebhook(cfg.WebhookURL, notify.Options{})
	case cfg.TelegramBotToken != "":
		return notify.NewTelegram("https://api.telegram.org", cfg.TelegramBotToken, cfg.TelegramChatID, notify.Options{})
	default:
		log.Warn("no notifier configured; alerts are only logged")
		return notify.NewLog(log.With("component", "notifier"))
	}
}
