// Package scaler tracks GitHub job state and drives a runner service's replica
// count towards it.
package scaler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ggoggam/railway-github-runner-autoscaler/internal/bounded"
)

// Backend applies an absolute replica count to the runner service.
type Backend interface {
	SetReplicas(ctx context.Context, n int) error
}

// trackedJobs is the number of terminal job IDs kept to guard against
// out-of-order webhook delivery.
const trackedJobs = 4096

// Options tunes scaling behaviour.
type Options struct {
	// MinReplicas is the floor held while idle. Zero scales to zero.
	MinReplicas int
	// MaxRunners is the ceiling on concurrent replicas.
	MaxRunners int
	// IdleCooldown is the quiet period required before shrinking the pool.
	IdleCooldown time.Duration
	// StartupGrace holds existing replicas after a restart, before any shrink.
	StartupGrace time.Duration
	// JobTTL forgets jobs whose completion webhook never arrived.
	JobTTL time.Duration
	// ResyncPeriod is how often to retry failed scales and expire stale jobs.
	ResyncPeriod time.Duration
}

// Autoscaler tracks jobs and reconciles the replica count.
//
// All Backend calls happen on a single goroutine in Run. Event methods only
// mutate in-memory counters and poke a trigger channel, so concurrent webhook
// deliveries can never interleave a read-modify-write against the API.
type Autoscaler struct {
	opts    Options
	backend Backend
	logger  *slog.Logger
	now     func() time.Time

	mu         sync.Mutex
	queued     map[int64]time.Time
	inProgress map[int64]time.Time
	done       *bounded.Set[int64]
	lastBusy   time.Time
	startedAt  time.Time
	applied    int // replica count last applied; -1 when unknown

	trigger chan struct{}
}

// New returns an Autoscaler that drives backend towards observed job demand.
func New(opts Options, backend Backend, logger *slog.Logger) *Autoscaler {
	a := &Autoscaler{
		opts:       opts,
		backend:    backend,
		logger:     logger,
		now:        time.Now,
		queued:     make(map[int64]time.Time),
		inProgress: make(map[int64]time.Time),
		done:       bounded.NewSet[int64](trackedJobs),
		applied:    -1,
		trigger:    make(chan struct{}, 1),
	}
	a.lastBusy = a.now()
	a.startedAt = a.lastBusy
	return a
}

// SetBaseline records the replica count the service already has, so a freshly
// started autoscaler knows what it is starting from rather than guessing.
//
// State lives in memory, so a restart forgets which jobs were running. The
// startup grace in decide keeps the existing replicas alive long enough for
// those jobs to finish or to re-announce themselves via webhooks.
func (a *Autoscaler) SetBaseline(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied = n
	a.lastBusy = a.now()
}

// OnQueued records a job waiting for a runner.
func (a *Autoscaler) OnQueued(id int64) {
	a.mu.Lock()
	if a.done.Has(id) {
		a.mu.Unlock()
		a.logger.Debug("ignoring queued for already-completed job", "job", id)
		return
	}
	a.queued[id] = a.now()
	a.lastBusy = a.now()
	q, ip := len(a.queued), len(a.inProgress)
	a.mu.Unlock()

	a.logger.Info("job queued", "job", id, "queued", q, "inProgress", ip)
	a.poke()
}

// OnInProgress records a job that a runner has picked up.
func (a *Autoscaler) OnInProgress(id int64) {
	a.mu.Lock()
	if a.done.Has(id) {
		a.mu.Unlock()
		a.logger.Debug("ignoring in_progress for already-completed job", "job", id)
		return
	}
	delete(a.queued, id)
	a.inProgress[id] = a.now()
	a.lastBusy = a.now()
	q, ip := len(a.queued), len(a.inProgress)
	a.mu.Unlock()

	a.logger.Info("job in progress", "job", id, "queued", q, "inProgress", ip)
	a.poke()
}

// OnCompleted records a finished job.
func (a *Autoscaler) OnCompleted(id int64) {
	a.mu.Lock()
	delete(a.queued, id)
	delete(a.inProgress, id)
	a.done.Add(id)
	a.lastBusy = a.now()
	q, ip := len(a.queued), len(a.inProgress)
	a.mu.Unlock()

	a.logger.Info("job completed", "job", id, "queued", q, "inProgress", ip)
	a.poke()
}

// poke requests a reconcile without blocking the caller.
func (a *Autoscaler) poke() {
	select {
	case a.trigger <- struct{}{}:
	default: // a reconcile is already pending; it will see the latest state
	}
}

// Run drives reconciliation until ctx is cancelled. It is the only goroutine
// permitted to call the Backend.
func (a *Autoscaler) Run(ctx context.Context) {
	ticker := time.NewTicker(a.opts.ResyncPeriod)
	defer ticker.Stop()

	for {
		a.reconcile(ctx)

		select {
		case <-ctx.Done():
			return
		case <-a.trigger:
		case <-ticker.C:
		}
	}
}

// decision is the outcome of evaluating current state against desired state.
type decision struct {
	desired    int
	queued     int
	inProgress int
	// heldDown is set when a scale-down was deliberately deferred. The resync
	// ticker will re-evaluate.
	heldDown bool
	reason   string
}

func (a *Autoscaler) decide() decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	q, ip := len(a.queued), len(a.inProgress)
	want := clamp(q+ip, a.opts.MinReplicas, a.opts.MaxRunners)
	d := decision{desired: want, queued: q, inProgress: ip}

	// Scaling down is the dangerous direction: Railway picks which replica to
	// terminate, so shrinking while a runner is mid-job kills that job.
	if a.applied >= 0 && want < a.applied {
		now := a.now()
		switch {
		case ip > 0:
			d.desired, d.heldDown, d.reason = a.applied, true, "jobs still in progress"
		case now.Sub(a.lastBusy) < a.opts.IdleCooldown:
			// Absorbs webhook lag: a job can be picked up by a runner before
			// its in_progress event arrives.
			d.desired, d.heldDown, d.reason = a.applied, true, "idle cooldown not elapsed"
		case now.Sub(a.startedAt) < a.opts.StartupGrace:
			// Just booted, so an empty job map means "we have not heard yet",
			// not "nothing is running". Shrinking now could kill live jobs.
			d.desired, d.heldDown, d.reason = a.applied, true, "startup grace not elapsed"
		}
	}
	return d
}

func (a *Autoscaler) reconcile(ctx context.Context) {
	a.gc()

	d := a.decide()

	a.mu.Lock()
	applied := a.applied
	a.mu.Unlock()

	if d.heldDown {
		a.logger.Debug("scale-down deferred",
			"reason", d.reason, "replicas", applied, "queued", d.queued, "inProgress", d.inProgress)
		return
	}
	if d.desired == applied {
		return
	}

	if err := a.backend.SetReplicas(ctx, d.desired); err != nil {
		// Leave applied untouched so the next tick retries.
		a.logger.Error("set replicas failed",
			"desired", d.desired, "current", applied, "err", err)
		return
	}

	a.mu.Lock()
	a.applied = d.desired
	a.mu.Unlock()

	direction := "scaled up"
	if applied >= 0 && d.desired < applied {
		direction = "scaled down"
	}
	a.logger.Info(direction,
		"replicas", d.desired, "from", applied, "queued", d.queued, "inProgress", d.inProgress)
}

// gc drops jobs whose terminal webhook never arrived. Without it a single lost
// "completed" delivery would pin the replica count up forever.
func (a *Autoscaler) gc() {
	if a.opts.JobTTL <= 0 {
		return
	}
	cutoff := a.now().Add(-a.opts.JobTTL)

	a.mu.Lock()
	var expired []int64
	for id, at := range a.queued {
		if at.Before(cutoff) {
			delete(a.queued, id)
			expired = append(expired, id)
		}
	}
	for id, at := range a.inProgress {
		if at.Before(cutoff) {
			delete(a.inProgress, id)
			expired = append(expired, id)
		}
	}
	a.mu.Unlock()

	if len(expired) > 0 {
		a.logger.Warn("expired stale jobs (missing completion webhook?)",
			"jobs", expired, "ttl", a.opts.JobTTL)
	}
}

// Stats is a point-in-time snapshot, served by the /status endpoint.
type Stats struct {
	Queued      int    `json:"queued"`
	InProgress  int    `json:"inProgress"`
	Replicas    int    `json:"replicas"`
	MinReplicas int    `json:"minReplicas"`
	MaxRunners  int    `json:"maxRunners"`
	LastBusy    string `json:"lastBusy"`
}

// Stats returns the current job counts and replica state.
func (a *Autoscaler) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Stats{
		Queued:      len(a.queued),
		InProgress:  len(a.inProgress),
		Replicas:    a.applied,
		MinReplicas: a.opts.MinReplicas,
		MaxRunners:  a.opts.MaxRunners,
		LastBusy:    a.lastBusy.UTC().Format(time.RFC3339),
	}
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
