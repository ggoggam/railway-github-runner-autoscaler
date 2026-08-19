package config

import (
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/github"
	"reflect"
	"testing"
	"time"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/railway"
)

// setRequiredEnv sets the minimum viable configuration.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_WEBHOOK_SECRET", "secret")
	t.Setenv("RAILWAY_API_TOKEN", "token")
	t.Setenv("RAILWAY_RUNNER_SERVICE_ID", "svc")
	t.Setenv("RAILWAY_ENVIRONMENT_ID", "env")
}

func TestLoadDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxRunners != DefaultMaxRunners {
		t.Errorf("MaxRunners = %d, want %d", cfg.MaxRunners, DefaultMaxRunners)
	}
	if cfg.MinReplicas != DefaultMinReplicas {
		t.Errorf("MinReplicas = %d, want %d", cfg.MinReplicas, DefaultMinReplicas)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %q, want %q", cfg.Port, DefaultPort)
	}
	if cfg.APIEndpoint != railway.DefaultEndpoint {
		t.Errorf("APIEndpoint = %q, want %q", cfg.APIEndpoint, railway.DefaultEndpoint)
	}
	if !reflect.DeepEqual(cfg.RunnerLabels, []string{"self-hosted", "railway"}) {
		t.Errorf("RunnerLabels = %v", cfg.RunnerLabels)
	}
}

func TestLoadRequiresEachSecret(t *testing.T) {
	for _, missing := range []string{
		"GITHUB_WEBHOOK_SECRET",
		"RAILWAY_API_TOKEN",
		"RAILWAY_RUNNER_SERVICE_ID",
		"RAILWAY_ENVIRONMENT_ID",
	} {
		t.Run(missing, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(missing, "")
			if _, err := Load(); err == nil {
				t.Fatalf("want error when %s is unset", missing)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MAX_RUNNERS", "12")
	t.Setenv("MIN_REPLICAS", "0")
	t.Setenv("PORT", "9090")
	t.Setenv("RUNNER_LABELS", " Railway , Self-Hosted ")
	t.Setenv("IDLE_COOLDOWN", "90s")
	t.Setenv("STARTUP_GRACE", "10m")
	t.Setenv("JOB_TTL", "2h")
	t.Setenv("RAILWAY_RUNNER_REGION", "us-west2")
	t.Setenv("RAILWAY_API_URL", "http://localhost:9999/graphql/v2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxRunners != 12 || cfg.MinReplicas != 0 || cfg.Port != "9090" {
		t.Errorf("scalar overrides not applied: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.RunnerLabels, []string{"railway", "self-hosted"}) {
		t.Errorf("labels not normalized: %v", cfg.RunnerLabels)
	}
	if cfg.IdleCooldown != 90*time.Second || cfg.StartupGrace != 10*time.Minute || cfg.JobTTL != 2*time.Hour {
		t.Errorf("durations not applied: %+v", cfg)
	}
	if cfg.Region != "us-west2" {
		t.Errorf("Region = %q", cfg.Region)
	}
	if cfg.APIEndpoint != "http://localhost:9999/graphql/v2" {
		t.Errorf("APIEndpoint = %q", cfg.APIEndpoint)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"max runners zero":     {"MAX_RUNNERS": "0"},
		"max runners negative": {"MAX_RUNNERS": "-1"},
		"max runners garbage":  {"MAX_RUNNERS": "many"},
		"min above max":        {"MIN_REPLICAS": "5", "MAX_RUNNERS": "2"},
		"min negative":         {"MIN_REPLICAS": "-1"},
		"bad duration":         {"IDLE_COOLDOWN": "soon"},
		"zero resync":          {"RESYNC_PERIOD": "0s"},
		"blank labels":         {"RUNNER_LABELS": " , , "},
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("want error for %s", name)
			}
		})
	}
}

func TestProjectionsCarryValues(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MAX_RUNNERS", "7")
	t.Setenv("MIN_REPLICAS", "2")
	t.Setenv("IDLE_COOLDOWN", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	so := cfg.ScalerOptions()
	if so.MaxRunners != 7 || so.MinReplicas != 2 || so.IdleCooldown != 45*time.Second {
		t.Errorf("ScalerOptions lost values: %+v", so)
	}

	wo := cfg.WebhookOptions()
	if wo.Secret != "secret" || !reflect.DeepEqual(wo.Labels, cfg.RunnerLabels) {
		t.Errorf("WebhookOptions lost values: %+v", wo)
	}
}

func TestNormalizeLabels(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"railway", []string{"railway"}},
		{"A, B ,c", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
		{"  ", nil},
	} {
		if got := NormalizeLabels(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("NormalizeLabels(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Reconciling against GitHub needs both a credential and a repository. Taking
// one without the other would quietly leave the autoscaler webhook-only, which
// is the failure mode this feature exists to remove.
func TestGitHubReconcileRequiresBothTokenAndRepository(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"token without repository": {"GITHUB_API_TOKEN": "ghp_x"},
		"repository without token": {"GITHUB_API_REPOSITORY": "acme/widgets"},
		"malformed repository":     {"GITHUB_API_TOKEN": "ghp_x", "GITHUB_REPOSITORY": "acme"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

func TestGitHubReconcileOptional(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubToken != "" || cfg.Repository != "" {
		t.Fatalf("want reconciliation disabled by default, got %+v", cfg)
	}
	if cfg.RunnerGrace != DefaultRunnerGrace {
		t.Fatalf("want RunnerGrace default %v, got %v", DefaultRunnerGrace, cfg.RunnerGrace)
	}
	if cfg.GitHubAPIURL == "" {
		t.Fatal("want a GitHub API endpoint default")
	}
}

func TestGitHubReconcileEnabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GITHUB_API_TOKEN", "ghp_x")
	t.Setenv("GITHUB_API_REPOSITORY", "acme/widgets")
	t.Setenv("RUNNER_GRACE", "90s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repository != "acme/widgets" {
		t.Fatalf("got repository %q", cfg.Repository)
	}
	if cfg.ScalerOptions().RunnerGrace != 90*time.Second {
		t.Fatalf("got RunnerGrace %v", cfg.ScalerOptions().RunnerGrace)
	}
}

// GitHub Actions injects GITHUB_REPOSITORY, GITHUB_TOKEN, and GITHUB_API_URL
// into every workflow step. Reading those names would mean any Actions context
// silently supplies half a configuration — which is exactly how this config
// first broke its own CI.
func TestActionsInjectedNamesAreIgnored(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GITHUB_REPOSITORY", "acme/injected-by-actions")
	t.Setenv("GITHUB_TOKEN", "injected-by-actions")
	t.Setenv("GITHUB_API_URL", "https://api.github.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Actions' own env must not affect Load: %v", err)
	}
	if cfg.Repository != "" || cfg.GitHubToken != "" {
		t.Fatalf("picked up Actions-injected config: %+v", cfg)
	}
	if cfg.GitHubAPIURL != github.DefaultEndpoint {
		t.Fatalf("picked up Actions-injected API URL: %q", cfg.GitHubAPIURL)
	}
}
