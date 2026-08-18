// Package webhook serves GitHub workflow_job events and the service's
// observability endpoints.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/bounded"
	"github.com/ggoggam/railway-github-runner-autoscaler/internal/scaler"
)

const (
	maxBodyBytes      = 5 << 20 // 5MiB
	trackedDeliveries = 4096
)

// Tracker consumes job lifecycle events and reports current state.
type Tracker interface {
	OnQueued(id int64)
	OnInProgress(id int64)
	OnCompleted(id int64)
	Stats() scaler.Stats
}

// Options configures webhook handling.
type Options struct {
	// Secret is the shared secret GitHub signs payloads with.
	Secret string
	// Labels must all be present on a job for it to be counted.
	Labels []string
}

// jobEvent is the slice of GitHub's workflow_job payload we care about.
type jobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID     int64    `json:"id"`
		Labels []string `json:"labels"`
	} `json:"workflow_job"`
}

// Handler serves the webhook and observability endpoints.
type Handler struct {
	opts    Options
	tracker Tracker
	logger  *slog.Logger

	mu         sync.Mutex
	deliveries *bounded.Set[string]
}

// New returns a Handler feeding events to tracker.
func New(opts Options, tracker Tracker, logger *slog.Logger) *Handler {
	return &Handler{
		opts:       opts,
		tracker:    tracker,
		logger:     logger,
		deliveries: bounded.NewSet[string](trackedDeliveries),
	}
}

// Routes returns the HTTP routes this handler serves.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.handleWebhook)
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("GET /status", h.handleStatus)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.tracker.Stats())
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	if !ValidateSignature(body, r.Header.Get("X-Hub-Signature-256"), h.opts.Secret) {
		h.logger.Warn("rejected webhook with invalid signature", "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "workflow_job":
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	default:
		// Signature checked out, so ack rather than making GitHub retry.
		w.WriteHeader(http.StatusOK)
		return
	}

	// GitHub retries deliveries; the same delivery ID must not be counted twice.
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		h.mu.Lock()
		seen := h.deliveries.Add(id)
		h.mu.Unlock()
		if seen {
			h.logger.Debug("ignoring duplicate delivery", "delivery", id)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	var ev jobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if !MatchesLabels(ev.WorkflowJob.Labels, h.opts.Labels) {
		h.logger.Debug("ignoring job for other runners",
			"job", ev.WorkflowJob.ID, "labels", ev.WorkflowJob.Labels, "want", h.opts.Labels)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch ev.Action {
	case "queued":
		h.tracker.OnQueued(ev.WorkflowJob.ID)
	case "in_progress":
		h.tracker.OnInProgress(ev.WorkflowJob.ID)
	case "completed":
		h.tracker.OnCompleted(ev.WorkflowJob.ID)
	default:
		h.logger.Debug("ignoring unhandled action", "action", ev.Action)
	}

	// Reconciliation is asynchronous, so the ack never depends on Railway being
	// reachable. A failed scale is retried by the resync loop instead of
	// relying on GitHub redelivering the webhook.
	w.WriteHeader(http.StatusOK)
}

// ValidateSignature verifies GitHub's HMAC-SHA256 body signature.
func ValidateSignature(body []byte, sigHeader, secret string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	provided, err := hex.DecodeString(sigHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// MatchesLabels reports whether the job requests every configured runner label.
func MatchesLabels(jobLabels, required []string) bool {
	if len(required) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(jobLabels))
	for _, l := range jobLabels {
		have[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	for _, want := range required {
		if _, ok := have[want]; !ok {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
