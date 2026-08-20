// Package config loads and validates the autoscaler's environment configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/github"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/railway"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/scaler"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/webhook"
)

// Config holds every tunable the autoscaler reads from the environment.
type Config struct {
	// GitHub
	WebhookSecret string
	RunnerLabels  []string
	// RunnerScope mirrors RUNNER_SCOPE on myoung34/github-runner: ScopeRepo or
	// ScopeOrg. It decides how Repository is read, so one variable can feed both
	// the runner's REPO_URL and its ORG_NAME.
	RunnerScope string
	// Repository is owner/repo at repo scope and a bare organization name at org
	// scope. Together with GitHubToken it enables reconciliation against GitHub;
	// leaving both empty runs the autoscaler webhook-only. Note that GitHub has
	// no org-level jobs API, so at org scope only runner liveness is reconciled
	// and job state stays webhook-driven.
	//
	// These deliberately avoid the GITHUB_TOKEN / GITHUB_REPOSITORY /
	// GITHUB_API_URL names: GitHub Actions injects those into every workflow
	// step, so reusing them means any Actions context silently supplies half a
	// configuration.
	GitHubToken  string
	Repository   string
	GitHubAPIURL string
	RunnerGrace  time.Duration

	// Railway
	RailwayToken string
	// ServiceID is the runner service to scale. Empty means resolve ServiceName
	// against ProjectID at startup, which is what lets a template ship without
	// an ID that does not exist until it is deployed.
	ServiceID     string
	ServiceName   string
	ProjectID     string
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

// Runner scopes, matching the RUNNER_SCOPE values the runner image accepts.
const (
	ScopeRepo = "repo"
	ScopeOrg  = "org"
)

// Defaults applied when the corresponding variable is unset.
const (
	DefaultMaxRunners = 3
	// DefaultMinReplicas is 0: idle costs nothing, at the price of a cold start
	// on the first job. Raise it to keep a runner warm.
	DefaultMinReplicas = 0
	DefaultPort        = "8080"
	// DefaultRunnerScope matches the runner image's own default.
	DefaultRunnerScope  = ScopeRepo
	DefaultRunnerLabels = "self-hosted,railway"
	// DefaultServiceName matches the service name the Railway template gives the
	// runner pool.
	DefaultServiceName  = "github-runner"
	DefaultIdleCooldown = 60 * time.Second
	DefaultJobTTL       = 6 * time.Hour
	DefaultResyncPeriod = 30 * time.Second
	DefaultStartupGrace = 5 * time.Minute
	// DefaultRunnerGrace comfortably exceeds the time a runner container needs
	// to boot and register, so a pool mid-restart is never mistaken for a dead
	// one.
	DefaultRunnerGrace = 3 * time.Minute
)

// Load reads configuration from the environment, applying defaults and
// rejecting values that would misconfigure scaling.
func Load() (Config, error) {
	cfg := Config{
		MaxRunners:   DefaultMaxRunners,
		MinReplicas:  DefaultMinReplicas,
		Port:         DefaultPort,
		IdleCooldown: DefaultIdleCooldown,
		JobTTL:       DefaultJobTTL,
		ResyncPeriod: DefaultResyncPeriod,
		StartupGrace: DefaultStartupGrace,
		RunnerGrace:  DefaultRunnerGrace,
	}

	if err := rejectRemovedVars(); err != nil {
		return Config{}, err
	}

	// Required.
	for _, req := range []struct {
		key string
		dst *string
	}{
		{"GITHUB_WEBHOOK_SECRET", &cfg.WebhookSecret},
		{"RAILWAY_API_TOKEN", &cfg.RailwayToken},
		// Railway injects this automatically; the GraphQL mutation rejects an empty value.
		{"RAILWAY_ENVIRONMENT_ID", &cfg.EnvironmentID},
	} {
		v := strings.TrimSpace(os.Getenv(req.key))
		if v == "" {
			return Config{}, fmt.Errorf("%s is required", req.key)
		}
		*req.dst = v
	}

	// The runner service is addressed either by ID or by name. An explicit ID
	// wins; otherwise the name is resolved against the project at startup, which
	// needs the project ID Railway injects.
	cfg.ServiceID = strings.TrimSpace(os.Getenv("RAILWAY_RUNNER_SERVICE_ID"))
	cfg.ServiceName = strings.TrimSpace(os.Getenv("RAILWAY_RUNNER_SERVICE_NAME"))
	cfg.ProjectID = strings.TrimSpace(os.Getenv("RAILWAY_PROJECT_ID"))
	if cfg.ServiceID == "" {
		if cfg.ServiceName == "" {
			cfg.ServiceName = DefaultServiceName
		}
		if cfg.ProjectID == "" {
			return Config{}, fmt.Errorf(
				"RAILWAY_PROJECT_ID is required to resolve RAILWAY_RUNNER_SERVICE_NAME (%q); "+
					"set RAILWAY_RUNNER_SERVICE_ID instead to skip the lookup", cfg.ServiceName)
		}
	}

	// Optional. Empty means "discover from the service's current deployment".
	cfg.Region = strings.TrimSpace(os.Getenv("RAILWAY_RUNNER_REGION"))

	// The scope decides how GITHUB_API_REPOSITORY is read, which is what lets a
	// single variable feed both REPO_URL and ORG_NAME on the runner service.
	cfg.RunnerScope = strings.ToLower(strings.TrimSpace(os.Getenv("GITHUB_RUNNER_SCOPE")))
	if cfg.RunnerScope == "" {
		cfg.RunnerScope = DefaultRunnerScope
	}
	switch cfg.RunnerScope {
	case ScopeRepo, ScopeOrg:
	case "ent", "enterprise":
		return Config{}, fmt.Errorf("GITHUB_RUNNER_SCOPE=%q is not supported: "+
			"the enterprise runner APIs are not implemented", cfg.RunnerScope)
	default:
		return Config{}, fmt.Errorf("GITHUB_RUNNER_SCOPE must be %q or %q, got %q",
			ScopeRepo, ScopeOrg, cfg.RunnerScope)
	}

	// Optional, but paired: reconciling against GitHub needs a credential and
	// something to ask about. Accepting a partial configuration would silently
	// leave the autoscaler webhook-only.
	cfg.GitHubToken = strings.TrimSpace(os.Getenv("GITHUB_ACCESS_TOKEN"))
	cfg.Repository = strings.TrimSpace(os.Getenv("GITHUB_API_REPOSITORY"))
	switch {
	case cfg.GitHubToken != "" && cfg.Repository == "":
		return Config{}, fmt.Errorf("GITHUB_API_REPOSITORY (%s) is required when GITHUB_ACCESS_TOKEN is set",
			repositoryShape(cfg.RunnerScope))
	case cfg.Repository != "" && cfg.GitHubToken == "":
		return Config{}, fmt.Errorf("GITHUB_ACCESS_TOKEN is required when GITHUB_API_REPOSITORY is set")
	case cfg.Repository != "" && cfg.RunnerScope == ScopeRepo:
		if _, _, err := github.SplitRepository(cfg.Repository); err != nil {
			return Config{}, fmt.Errorf("GITHUB_API_REPOSITORY: %w at GITHUB_RUNNER_SCOPE=%s", err, ScopeRepo)
		}
	case cfg.Repository != "" && cfg.RunnerScope == ScopeOrg:
		if strings.Contains(cfg.Repository, "/") {
			return Config{}, fmt.Errorf("GITHUB_API_REPOSITORY must be a bare organization name at "+
				"GITHUB_RUNNER_SCOPE=%s, got %q", ScopeOrg, cfg.Repository)
		}
	}

	cfg.GitHubAPIURL = strings.TrimSpace(os.Getenv("GITHUB_API_ENDPOINT"))
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = github.DefaultEndpoint
	}

	// Overridable mainly for tests and proxies; defaults to Railway's public API.
	cfg.APIEndpoint = strings.TrimSpace(os.Getenv("RAILWAY_API_URL"))
	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = railway.DefaultEndpoint
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
		{"RUNNER_GRACE", &cfg.RunnerGrace},
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

	labels := os.Getenv("GITHUB_RUNNER_LABELS")
	if strings.TrimSpace(labels) == "" {
		labels = DefaultRunnerLabels
	}
	cfg.RunnerLabels = NormalizeLabels(labels)
	if len(cfg.RunnerLabels) == 0 {
		return Config{}, fmt.Errorf("GITHUB_RUNNER_LABELS must contain at least one non-empty label")
	}

	return cfg, nil
}

// ScalerOptions projects the config onto the scaler package's options.
func (c Config) ScalerOptions() scaler.Options {
	return scaler.Options{
		MinReplicas:  c.MinReplicas,
		MaxRunners:   c.MaxRunners,
		IdleCooldown: c.IdleCooldown,
		StartupGrace: c.StartupGrace,
		JobTTL:       c.JobTTL,
		ResyncPeriod: c.ResyncPeriod,
		RunnerGrace:  c.RunnerGrace,
	}
}

// WebhookOptions projects the config onto the webhook package's options.
func (c Config) WebhookOptions() webhook.Options {
	return webhook.Options{
		Secret: c.WebhookSecret,
		Labels: c.RunnerLabels,
	}
}

// NormalizeLabels splits a comma-separated label list, lowercasing and
// dropping blanks so matching is stable regardless of how it was written.
func NormalizeLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if l := strings.ToLower(strings.TrimSpace(part)); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// removedVars are names an earlier release read. Ignoring one silently would
// leave a live deployment reconciling nothing, so Load refuses to start and
// names the replacement instead.
var removedVars = []struct{ old, replacement string }{
	{"GITHUB_API_TOKEN", "GITHUB_ACCESS_TOKEN"},
	{"GITHUB_API_ORGANIZATION", "GITHUB_RUNNER_SCOPE=org with the organization name in GITHUB_API_REPOSITORY"},
	{"RUNNER_LABELS", "GITHUB_RUNNER_LABELS"},
}

func rejectRemovedVars() error {
	for _, v := range removedVars {
		if strings.TrimSpace(os.Getenv(v.old)) != "" {
			return fmt.Errorf("%s is no longer read; use %s", v.old, v.replacement)
		}
	}
	return nil
}

// repositoryShape describes what GITHUB_API_REPOSITORY must hold at a scope.
func repositoryShape(scope string) string {
	if scope == ScopeOrg {
		return "the organization name"
	}
	return "owner/repo"
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
