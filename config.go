package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the autoscaler reads from the environment.
type Config struct {
	// GitHub
	WebhookSecret string
	RunnerLabels  []string

	// Railway
	RailwayToken  string
	ServiceID     string
	EnvironmentID string
	Region        string
	APIEndpoint   string

	// Scaling
	MinReplicas  int
	MaxRunners   int
	IdleCooldown time.Duration
	JobTTL       time.Duration
	ResyncPeriod time.Duration
	StartupGrace time.Duration

	// Server
	Port string
}

const (
	defaultMaxRunners   = 3
	defaultMinReplicas  = 1
	defaultPort         = "8080"
	defaultRunnerLabels = "self-hosted,railway"
	defaultIdleCooldown = 60 * time.Second
	defaultJobTTL       = 6 * time.Hour
	defaultResyncPeriod = 30 * time.Second
	defaultStartupGrace = 5 * time.Minute
)

func loadConfig() (Config, error) {
	cfg := Config{
		MaxRunners:   defaultMaxRunners,
		MinReplicas:  defaultMinReplicas,
		Port:         defaultPort,
		IdleCooldown: defaultIdleCooldown,
		JobTTL:       defaultJobTTL,
		ResyncPeriod: defaultResyncPeriod,
		StartupGrace: defaultStartupGrace,
	}

	// Required.
	for _, req := range []struct {
		key string
		dst *string
	}{
		{"GITHUB_WEBHOOK_SECRET", &cfg.WebhookSecret},
		{"RAILWAY_API_TOKEN", &cfg.RailwayToken},
		{"RAILWAY_RUNNER_SERVICE_ID", &cfg.ServiceID},
		// Railway injects this automatically; the GraphQL mutation rejects an empty value.
		{"RAILWAY_ENVIRONMENT_ID", &cfg.EnvironmentID},
	} {
		v := strings.TrimSpace(os.Getenv(req.key))
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", req.key)
		}
		*req.dst = v
	}

	// Optional. Empty means "discover from the service's current deployment".
	cfg.Region = strings.TrimSpace(os.Getenv("RAILWAY_RUNNER_REGION"))

	// Overridable mainly for tests and proxies; defaults to Railway's public API.
	cfg.APIEndpoint = strings.TrimSpace(os.Getenv("RAILWAY_API_URL"))
	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = DefaultRailwayEndpoint
	}

	if err := envInt("MAX_RUNNERS", 1, &cfg.MaxRunners); err != nil {
		return Config{}, err
	}
	if err := envInt("MIN_REPLICAS", 0, &cfg.MinReplicas); err != nil {
		return Config{}, err
	}
	if cfg.MinReplicas > cfg.MaxRunners {
		return Config{}, fmt.Errorf("MIN_REPLICAS (%d) must not exceed MAX_RUNNERS (%d)", cfg.MinReplicas, cfg.MaxRunners)
	}

	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"IDLE_COOLDOWN", &cfg.IdleCooldown},
		{"JOB_TTL", &cfg.JobTTL},
		{"RESYNC_PERIOD", &cfg.ResyncPeriod},
		{"STARTUP_GRACE", &cfg.StartupGrace},
	} {
		if err := envDuration(d.key, d.dst); err != nil {
			return Config{}, err
		}
	}

	// Drives the ticker that retries failed scales and expires stale jobs.
	if cfg.ResyncPeriod <= 0 {
		return Config{}, fmt.Errorf("RESYNC_PERIOD must be greater than zero")
	}

	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		cfg.Port = v
	}

	labels := os.Getenv("RUNNER_LABELS")
	if strings.TrimSpace(labels) == "" {
		labels = defaultRunnerLabels
	}
	cfg.RunnerLabels = normalizeLabels(labels)
	if len(cfg.RunnerLabels) == 0 {
		return Config{}, fmt.Errorf("RUNNER_LABELS must contain at least one non-empty label")
	}

	return cfg, nil
}

// normalizeLabels splits a comma-separated label list, lowercasing and
// dropping blanks so matching is stable regardless of how it was written.
func normalizeLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if l := strings.ToLower(strings.TrimSpace(part)); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func envInt(key string, minimum int, dst *int) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum {
		return fmt.Errorf("%s must be an integer >= %d, got %q", key, minimum, v)
	}
	*dst = n
	return nil
}

func envDuration(key string, dst *time.Duration) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("%s must be a non-negative Go duration (e.g. 90s, 5m), got %q", key, v)
	}
	*dst = d
	return nil
}
