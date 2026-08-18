package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Scaler applies an absolute replica count to the runner service.
type Scaler interface {
	SetReplicas(ctx context.Context, n int) error
}

// trackedJobs is the number of terminal job IDs kept to guard against
// out-of-order webhook delivery.
const trackedJobs = 4096

// Autoscaler tracks GitHub job state and drives the runner service's replica
// count towards it.
//
// All Railway mutations happen on a single goroutine in Run. Webhook handlers
// only mutate in-memory counters and poke a trigger channel, so concurrent
// deliveries can never interleave a read-modify-write against the API.
type Autoscaler struct {
	cfg    Config
	scaler Scaler
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	queued     map[int64]time.Time
	inProgress map[int64]time.Time
	done       *boundedSet[int64]
	lastBusy   time.Time
	startedAt  time.Time
	applied    int // replica count last applied; -1 when unknown

	trigger chan struct{}
}

func NewAutoscaler(cfg Config, scaler Scaler, logger *slog.Logger) *Autoscaler {
	a := &Autoscaler{
		cfg:        cfg,
		scaler:     scaler,
		logger:     logger,
		now:        time.Now,
		queued:     make(map[int64]time.Time),
		inProgress: make(map[int64]time.Time),
		done:       newBoundedSet[int64](trackedJobs),
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
// startup grace in decide() keeps the existing replicas alive long enough for
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
// permitted to call the Scaler.
func (a *Autoscaler) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.ResyncPeriod)
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
	// heldDown is set when a scale-down was deliberately deferred, either
	// because jobs are still running or because the idle cooldown has not
	// elapsed. The resync ticker will re-evaluate.
	heldDown bool
	reason   string
}

func (a *Autoscaler) decide() decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	q, ip := len(a.queued), len(a.inProgress)
	want := clamp(q+ip, a.cfg.MinReplicas, a.cfg.MaxRunners)
	d := decision{desired: want, queued: q, inProgress: ip}

	// Scaling down is the dangerous direction: Railway picks which replica to
	// terminate, so shrinking while a runner is mid-job kills that job.
	if a.applied >= 0 && want < a.applied {
		now := a.now()
		switch {
		case ip > 0:
			d.desired, d.heldDown, d.reason = a.applied, true, "jobs still in progress"
		case now.Sub(a.lastBusy) < a.cfg.IdleCooldown:
			// Absorbs webhook lag: a job can be picked up by a runner before
			// its in_progress event arrives.
			d.desired, d.heldDown, d.reason = a.applied, true, "idle cooldown not elapsed"
		case now.Sub(a.startedAt) < a.cfg.StartupGrace:
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

	if err := a.scaler.SetReplicas(ctx, d.desired); err != nil {
		// Leave a.applied untouched so the next tick retries.
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
	if a.cfg.JobTTL <= 0 {
		return
	}
	cutoff := a.now().Add(-a.cfg.JobTTL)

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
			"jobs", expired, "ttl", a.cfg.JobTTL)
	}
}

// Stats is a point-in-time snapshot for the /status endpoint.
type Stats struct {
	Queued     int    `json:"queued"`
	InProgress int    `json:"inProgress"`
	Replicas   int    `json:"replicas"`
	MinRepl    int    `json:"minReplicas"`
	MaxRunners int    `json:"maxRunners"`
	LastBusy   string `json:"lastBusy"`
}

func (a *Autoscaler) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Stats{
		Queued:     len(a.queued),
		InProgress: len(a.inProgress),
		Replicas:   a.applied,
		MinRepl:    a.cfg.MinReplicas,
		MaxRunners: a.cfg.MaxRunners,
		LastBusy:   a.lastBusy.UTC().Format(time.RFC3339),
	}
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
