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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"

	"github.com/Amadeus-22/chainwatch/internal/adapter/ethrpc"
	"github.com/Amadeus-22/chainwatch/internal/adapter/httpapi"
	"github.com/Amadeus-22/chainwatch/internal/adapter/notify"
	"github.com/Amadeus-22/chainwatch/internal/adapter/postgres"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/platform/config"
	"github.com/Amadeus-22/chainwatch/internal/platform/logging"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

var version = "dev"

var _ httpapi.Store = (*postgres.Store)(nil)

func main() {
	if err := run(); err != nil {
		slog.Error("chainwatch stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
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

	chain := ethrpc.New(cfg.RPCURL, cfg.RPCTimeout, m)
	indexer := app.NewIndexer(chain, store, app.IndexerConfig{
		Confirmations: cfg.Confirmations,
		ReorgWindow:   cfg.ReorgWindow,
		MaxBlockRange: cfg.MaxBlockRange,
		StartBlock:    cfg.StartBlock,
		PollInterval:  cfg.PollInterval,
		Risk:          risk.Params{LargeOutflow: cfg.LargeOutflow, DrainWindow: cfg.DrainWindow},
	}, log.With("component", "indexer"), m)
	alerter := app.NewAlerter(store, newNotifier(cfg, log), cfg.NotifyInterval, log.With("component", "alerter"), m)

	checks := []httpapi.Check{{Name: "postgres", Fn: store.Ping}, {Name: "rpc", Fn: chain.Ping}}
	server := httpapi.NewServer(cfg.HTTPAddr, httpapi.NewHandler(store, checks, reg, log.With("component", "http")))

	log.Info("chainwatch starting", "version", version, "http_addr", cfg.HTTPAddr,
		"confirmations", cfg.Confirmations, "reorg_window", cfg.ReorgWindow)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return indexer.Run(gctx) })
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
