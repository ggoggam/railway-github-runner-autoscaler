// Command autoscaler scales a pool of ephemeral self-hosted GitHub Actions
// runners on Railway in response to workflow_job webhooks.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/config"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/railway"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/scaler"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/webhook"
)

// railwayBackend binds a Railway client to one service instance.
type railwayBackend struct {
	client    *railway.Client
	serviceID string
	envID     string
	regions   []string
}

func (b *railwayBackend) SetReplicas(ctx context.Context, n int) error {
	return b.client.SetReplicas(ctx, b.serviceID, b.envID, b.regions, n)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := railway.NewClient(cfg.RailwayToken, logger)
	client.Endpoint = cfg.APIEndpoint

	backend := &railwayBackend{client: client, serviceID: cfg.ServiceID, envID: cfg.EnvironmentID}
	auto := scaler.New(cfg.ScalerOptions(), backend, logger)

	// Learn the service's current regions and replica count. Failure here is not
	// fatal: SetReplicas falls back to the legacy numReplicas field, and an
	// unknown baseline just means the first reconcile applies desired state.
	discoverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	state, err := client.DiscoverServiceState(discoverCtx, cfg.ServiceID, cfg.EnvironmentID)
	cancel()
	if err != nil {
		logger.Warn("service discovery failed, falling back to legacy numReplicas", "err", err)
	} else {
		backend.regions = state.Regions
		if state.Replicas >= 0 {
			auto.SetBaseline(state.Replicas)
		}
	}

	// An explicit region always wins over discovery.
	if cfg.Region != "" {
		backend.regions = []string{cfg.Region}
	}

	logger.Info("starting",
		"service", cfg.ServiceID,
		"environment", cfg.EnvironmentID,
		"regions", backend.regions,
		"minReplicas", cfg.MinReplicas,
		"maxRunners", cfg.MaxRunners,
		"labels", cfg.RunnerLabels,
		"idleCooldown", cfg.IdleCooldown,
		"startupGrace", cfg.StartupGrace,
		"resyncPeriod", cfg.ResyncPeriod,
		"jobTTL", cfg.JobTTL,
		"baselineReplicas", state.Replicas,
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		auto.Run(ctx)
	}()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           webhook.New(cfg.WebhookOptions(), auto, logger).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "err", err)
	}
	wg.Wait()
	logger.Info("stopped")
	return nil
}
