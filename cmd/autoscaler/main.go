// Command autoscaler scales a pool of ephemeral self-hosted GitHub Actions
// runners on Railway in response to workflow_job webhooks.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/config"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/github"
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

// githubObserver reads the pool's live runners — and, at repo scope, its
// outstanding jobs — filtered to the labels this pool serves.
type githubObserver struct {
	client *github.Client
	labels []string
}

func (o *githubObserver) Observe(ctx context.Context) (scaler.Observation, error) {
	var obs scaler.Observation

	runners, err := o.client.ListRunners(ctx)
	if err != nil {
		return obs, fmt.Errorf("list runners: %w", err)
	}
	for _, r := range runners {
		// A runner advertises its own labels, so the same label match used for
		// jobs decides whether it counts as capacity for this pool.
		if r.Online() && webhook.MatchesLabels(r.Labels, o.labels) {
			obs.LiveRunners++
		}
	}

	// Workflow runs live on repositories and GitHub has no org-level jobs
	// API, so at org scope job state stays webhook-driven and only runner
	// liveness is reconciled.
	if o.client.OrgScoped() {
		return obs, nil
	}

	jobs, err := o.client.ListActiveJobs(ctx)
	if err != nil {
		return obs, fmt.Errorf("list active jobs: %w", err)
	}
	obs.JobsObserved = true
	for _, j := range jobs {
		if !webhook.MatchesLabels(j.Labels, o.labels) {
			continue
		}
		switch j.Status {
		case "in_progress":
			obs.InProgress = append(obs.InProgress, j.ID)
		default: // "queued" and "waiting" both need a runner
			obs.Queued = append(obs.Queued, j.ID)
		}
	}
	return obs, nil
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

// resolveWait bounds how long startup waits for the runner service to become
// resolvable. A template creates every service at once, so the autoscaler can
// boot before the runner record is queryable; failing fast there would crash-loop
// a deploy that is actually fine.
const resolveWait = 2 * time.Minute

// resolveRetryInterval is how often the lookup is retried while waiting.
const resolveRetryInterval = 5 * time.Second

// resolveServiceID returns the runner service ID, looking it up by name when no
// explicit ID was configured.
func resolveServiceID(ctx context.Context, client *railway.Client, cfg config.Config, logger *slog.Logger) (string, error) {
	if cfg.ServiceID != "" {
		return cfg.ServiceID, nil
	}

	deadline := time.Now().Add(resolveWait)
	for {
		lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		id, err := client.ResolveServiceID(lookupCtx, cfg.ProjectID, cfg.ServiceName)
		cancel()
		if err == nil {
			logger.Info("resolved runner service", "name", cfg.ServiceName, "service", id)
			return id, nil
		}
		// Anything other than a missing service is a real misconfiguration
		// (bad token, wrong project) that waiting will not fix.
		if !errors.Is(err, railway.ErrServiceNotFound) {
			return "", fmt.Errorf("resolve runner service %q: %w", cfg.ServiceName, err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("resolve runner service %q: %w", cfg.ServiceName, err)
		}

		logger.Warn("runner service not found yet, retrying",
			"name", cfg.ServiceName, "retryIn", resolveRetryInterval, "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(resolveRetryInterval):
		}
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

	serviceID, err := resolveServiceID(ctx, client, cfg, logger)
	if err != nil {
		return err
	}

	backend := &railwayBackend{client: client, serviceID: serviceID, envID: cfg.EnvironmentID}
	auto := scaler.New(cfg.ScalerOptions(), backend, logger)

	// Optional: without a GitHub credential the autoscaler stays webhook-only,
	// which is how it behaved before and remains a valid way to run it.
	if cfg.GitHubToken != "" {
		var gh *github.Client
		if cfg.RunnerScope == config.ScopeOrg {
			gh = github.NewOrgClient(cfg.GitHubToken, cfg.Repository, logger)
			logger.Info("org scope: reconciling runner liveness only; job state stays webhook-driven",
				"organization", cfg.Repository)
		} else {
			owner, repo, err := github.SplitRepository(cfg.Repository)
			if err != nil {
				return err
			}
			gh = github.NewClient(cfg.GitHubToken, owner, repo, logger)
		}
		gh.Endpoint = cfg.GitHubAPIURL
		auto.SetObserver(&githubObserver{client: gh, labels: cfg.RunnerLabels})
	}

	// Learn the service's current regions and replica count. Failure here is not
	// fatal: SetReplicas falls back to the legacy numReplicas field, and an
	// unknown baseline just means the first reconcile applies desired state.
	discoverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	state, err := client.DiscoverServiceState(discoverCtx, serviceID, cfg.EnvironmentID)
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
		"service", serviceID,
		"environment", cfg.EnvironmentID,
		"regions", backend.regions,
		"minReplicas", cfg.MinReplicas,
		"maxRunners", cfg.MaxRunners,
		"labels", cfg.RunnerLabels,
		"idleCooldown", cfg.IdleCooldown,
		"startupGrace", cfg.StartupGrace,
		"resyncPeriod", cfg.ResyncPeriod,
		"jobTTL", cfg.JobTTL,
		"runnerGrace", cfg.RunnerGrace,
		"githubReconcile", cfg.GitHubToken != "",
		"runnerScope", cfg.RunnerScope,
		"githubTarget", cfg.Repository,
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
