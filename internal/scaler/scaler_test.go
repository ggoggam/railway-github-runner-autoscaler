package scaler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeBackend records every replica count applied to it.
type fakeBackend struct {
	mu    sync.Mutex
	calls []int
	err   error
}

func (f *fakeBackend) SetReplicas(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, n)
	return nil
}

func (f *fakeBackend) last() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return 0, false
	}
	return f.calls[len(f.calls)-1], true
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeBackend) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testOptions() Options {
	return Options{
		MinReplicas:  1,
		MaxRunners:   5,
		IdleCooldown: time.Minute,
		StartupGrace: 5 * time.Minute,
		JobTTL:       6 * time.Hour,
		ResyncPeriod: 30 * time.Second,
		RunnerGrace:  3 * time.Minute,
	}
}

// fakeObserver serves a canned view of GitHub.
type fakeObserver struct {
	mu    sync.Mutex
	obs   Observation
	err   error
	calls int
}

func (f *fakeObserver) Observe(context.Context) (Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Observation{}, f.err
	}
	return f.obs, nil
}

func (f *fakeObserver) set(o Observation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.obs = o
}

// newTestAutoscaler returns an autoscaler with a controllable clock, already
// past its startup grace and holding the given replica baseline.
func newTestAutoscaler(t *testing.T, opts Options, baseline int) (*Autoscaler, *fakeBackend, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fb := &fakeBackend{}
	a := New(opts, fb, discardLogger())
	a.now = func() time.Time { return clock }
	a.startedAt = clock
	a.lastBusy = clock
	if baseline >= 0 {
		a.SetBaseline(baseline)
	}
	// Move past the startup grace so tests exercise steady-state behaviour.
	clock = clock.Add(opts.StartupGrace + time.Second)
	return a, fb, &clock
}

func TestScalesUpWithQueuedJobs(t *testing.T) {
	a, fb, _ := newTestAutoscaler(t, testOptions(), 1)

	a.OnQueued(1)
	a.OnQueued(2)
	a.OnQueued(3)
	a.reconcile(t.Context())

	got, ok := fb.last()
	if !ok || got != 3 {
		t.Fatalf("want 3 replicas for 3 queued jobs, got %v (ok=%v)", got, ok)
	}
}

func TestScaleUpClampedToMaxRunners(t *testing.T) {
	opts := testOptions()
	opts.MaxRunners = 2
	a, fb, _ := newTestAutoscaler(t, opts, 1)

	for id := int64(1); id <= 6; id++ {
		a.OnQueued(id)
	}
	a.reconcile(t.Context())

	if got, _ := fb.last(); got != 2 {
		t.Fatalf("want replicas clamped to MaxRunners=2, got %d", got)
	}
}

// The reference implementation could shrink the pool while runners were still
// executing jobs, and Railway kills an arbitrary replica when it does.
func TestNeverScalesDownWhileJobsInProgress(t *testing.T) {
	a, fb, clock := newTestAutoscaler(t, testOptions(), 3)

	a.OnQueued(1)
	a.OnQueued(2)
	a.OnInProgress(1)
	a.OnCompleted(2) // one job done, job 1 still running

	*clock = clock.Add(time.Hour) // well past the idle cooldown
	a.reconcile(t.Context())

	if fb.count() != 0 {
		t.Fatalf("must not rescale while a job is in progress, got calls %v", fb.calls)
	}
}

func TestScaleDownWaitsForIdleCooldown(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 3)

	a.OnQueued(1)
	a.OnInProgress(1)
	a.OnCompleted(1)

	// Cooldown has not elapsed: hold the current replica count.
	a.reconcile(t.Context())
	if fb.count() != 0 {
		t.Fatalf("scaled down before cooldown elapsed: %v", fb.calls)
	}

	*clock = clock.Add(opts.IdleCooldown + time.Second)
	a.reconcile(t.Context())

	if got, ok := fb.last(); !ok || got != opts.MinReplicas {
		t.Fatalf("want scale down to MinReplicas=%d after cooldown, got %v (ok=%v)",
			opts.MinReplicas, got, ok)
	}
}

// A restarted autoscaler has no idea which jobs are running, so it must not
// immediately shrink a pool it found already scaled up.
func TestStartupGraceBlocksImmediateScaleDown(t *testing.T) {
	opts := testOptions()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fb := &fakeBackend{}
	a := New(opts, fb, discardLogger())
	a.now = func() time.Time { return clock }
	a.startedAt = clock
	a.lastBusy = clock
	a.SetBaseline(4)

	// Past the idle cooldown but still inside the startup grace.
	clock = clock.Add(opts.IdleCooldown + time.Second)
	a.reconcile(t.Context())
	if fb.count() != 0 {
		t.Fatalf("scaled down during startup grace: %v", fb.calls)
	}

	clock = clock.Add(opts.StartupGrace)
	a.reconcile(t.Context())
	if got, ok := fb.last(); !ok || got != opts.MinReplicas {
		t.Fatalf("want scale down after startup grace, got %v (ok=%v)", got, ok)
	}
}

// GitHub does not guarantee ordering; a late in_progress for a finished job
// must not resurrect it and pin the pool up forever.
func TestOutOfOrderInProgressAfterCompletedIsIgnored(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 2)

	a.OnQueued(1)
	a.OnCompleted(1)
	a.OnInProgress(1) // arrives late

	if s := a.Stats(); s.InProgress != 0 || s.Queued != 0 {
		t.Fatalf("late in_progress resurrected job: %+v", s)
	}

	*clock = clock.Add(opts.IdleCooldown + time.Second)
	a.reconcile(t.Context())
	if got, ok := fb.last(); !ok || got != opts.MinReplicas {
		t.Fatalf("want scale down to %d, got %v (ok=%v)", opts.MinReplicas, got, ok)
	}
}

// A lost "completed" delivery must not pin replicas up permanently.
func TestStaleJobsExpire(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 3)

	a.OnQueued(1)
	a.OnInProgress(1)

	*clock = clock.Add(opts.JobTTL + time.Minute)
	a.reconcile(t.Context())

	if s := a.Stats(); s.InProgress != 0 {
		t.Fatalf("stale job not expired: %+v", s)
	}
	if got, ok := fb.last(); !ok || got != opts.MinReplicas {
		t.Fatalf("want scale down to %d after expiry, got %v (ok=%v)", opts.MinReplicas, got, ok)
	}
}

func TestFailedScaleIsRetriedOnNextReconcile(t *testing.T) {
	a, fb, _ := newTestAutoscaler(t, testOptions(), 1)
	fb.setErr(errors.New("railway unavailable"))

	a.OnQueued(1)
	a.OnQueued(2)
	a.reconcile(t.Context())

	if got := a.Stats().Replicas; got != 1 {
		t.Fatalf("applied count must not advance past a failed scale, got %d", got)
	}

	fb.setErr(nil)
	a.reconcile(t.Context())
	if got, ok := fb.last(); !ok || got != 2 {
		t.Fatalf("want retry to apply 2 replicas, got %v (ok=%v)", got, ok)
	}
}

func TestNoRedundantCallsWhenDesiredMatchesApplied(t *testing.T) {
	a, fb, _ := newTestAutoscaler(t, testOptions(), 1)

	a.OnQueued(1)
	a.reconcile(t.Context())
	before := fb.count()

	a.reconcile(t.Context())
	a.reconcile(t.Context())

	if fb.count() != before {
		t.Fatalf("reconcile issued redundant API calls: %v", fb.calls)
	}
}

func TestMinReplicasZeroScalesToZeroWhenIdle(t *testing.T) {
	opts := testOptions()
	opts.MinReplicas = 0
	a, fb, clock := newTestAutoscaler(t, opts, 2)

	a.OnQueued(1)
	a.OnCompleted(1)
	*clock = clock.Add(opts.IdleCooldown + time.Second)
	a.reconcile(t.Context())

	if got, ok := fb.last(); !ok || got != 0 {
		t.Fatalf("want scale to 0 when MinReplicas=0 and idle, got %v (ok=%v)", got, ok)
	}
}

// The bug this rewrite exists for: concurrent webhook deliveries used to race
// on a read-modify-write against the Railway API. Run with -race.
func TestConcurrentWebhooksDoNotRace(t *testing.T) {
	a, _, _ := newTestAutoscaler(t, testOptions(), 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			a.OnQueued(id)
			a.OnInProgress(id)
			a.OnCompleted(id)
		}(int64(i))
	}
	wg.Wait()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}

	if s := a.Stats(); s.Queued != 0 || s.InProgress != 0 {
		t.Fatalf("counters leaked after all jobs completed: %+v", s)
	}
}

func TestClamp(t *testing.T) {
	for _, tc := range []struct{ v, lo, hi, want int }{
		{5, 1, 3, 3},
		{0, 1, 3, 1},
		{2, 1, 3, 2},
		{0, 0, 3, 0},
	} {
		if got := clamp(tc.v, tc.lo, tc.hi); got != tc.want {
			t.Errorf("clamp(%d,%d,%d) = %d, want %d", tc.v, tc.lo, tc.hi, got, tc.want)
		}
	}
}

// The 2026-08-19 stall: a lost "queued" delivery left the pool idle, then the
// only runner exited without its replica being rebuilt. The autoscaler held
// applied=1 and wanted 1, so every reconcile was a silent no-op and CI sat
// queued for three and a half hours.
func TestRecyclesPoolWithReplicasButNoLiveRunners(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 1)

	obs := &fakeObserver{}
	obs.set(Observation{LiveRunners: 0}) // replica exists, nothing registered
	a.SetObserver(obs)

	// Desired == applied, so without the watchdog this does nothing at all.
	a.reconcile(t.Context())
	if n := fb.count(); n != 0 {
		t.Fatalf("want no scaling before the grace elapses, got %d calls", n)
	}

	*clock = clock.Add(opts.RunnerGrace + time.Second)
	a.reconcile(t.Context())

	if got := fb.calls; len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("want a recycle through zero back to 1, got %v", got)
	}
	if s := a.Stats(); s.Replicas != 1 {
		t.Fatalf("want applied restored to 1 after recycle, got %d", s.Replicas)
	}
}

func TestNoRecycleWhileARunnerIsLive(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 1)

	obs := &fakeObserver{}
	obs.set(Observation{LiveRunners: 1})
	a.SetObserver(obs)

	*clock = clock.Add(opts.RunnerGrace * 10)
	a.reconcile(t.Context())

	if n := fb.count(); n != 0 {
		t.Fatalf("want no recycle while a runner is live, got %d calls", n)
	}
}

// Recycling goes through zero, so it must never fire while the shortfall could
// be a runner that is simply mid-job.
func TestNoRecycleOnPartialShortfall(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 3)

	obs := &fakeObserver{}
	obs.set(Observation{LiveRunners: 1, InProgress: []int64{1, 2, 3}})
	a.SetObserver(obs)

	*clock = clock.Add(opts.RunnerGrace * 10)
	a.reconcile(t.Context())

	for _, n := range fb.calls {
		if n == 0 {
			t.Fatalf("want no scale to zero on a partial shortfall, got %v", fb.calls)
		}
	}
}

// GitHub never redelivers a failed webhook, so a dropped "queued" event is
// invisible to the in-memory view forever.
func TestAdoptsQueuedJobsMissedByWebhooks(t *testing.T) {
	a, fb, _ := newTestAutoscaler(t, testOptions(), 1)

	obs := &fakeObserver{}
	obs.set(Observation{Queued: []int64{7, 8, 9}, LiveRunners: 1})
	a.SetObserver(obs)

	a.reconcile(t.Context())

	if got, ok := fb.last(); !ok || got != 3 {
		t.Fatalf("want scale to 3 for 3 adopted jobs, got %v (ok=%v)", got, ok)
	}
	if s := a.Stats(); s.Queued != 3 {
		t.Fatalf("want 3 tracked queued jobs, got %d", s.Queued)
	}
}

// A lost "completed" delivery would otherwise pin the pool up until JobTTL.
func TestDropsJobsGitHubNoLongerReports(t *testing.T) {
	opts := testOptions()
	a, _, clock := newTestAutoscaler(t, opts, 2)

	a.OnQueued(1)
	obs := &fakeObserver{}
	obs.set(Observation{LiveRunners: 1}) // GitHub says nothing is outstanding
	a.SetObserver(obs)

	// Fresh jobs are held: the API can lag a webhook we just accepted.
	a.reconcile(t.Context())
	if s := a.Stats(); s.Queued != 1 {
		t.Fatalf("want the fresh job retained, got %d queued", s.Queued)
	}

	*clock = clock.Add(opts.IdleCooldown + time.Second)
	a.reconcile(t.Context())
	if s := a.Stats(); s.Queued != 0 {
		t.Fatalf("want the stale job dropped once GitHub disowns it, got %d queued", s.Queued)
	}
}

// Observation is a correction, not a dependency: GitHub being unreachable must
// not stop webhooks from driving the pool.
func TestObserverFailureFallsBackToWebhooks(t *testing.T) {
	a, fb, _ := newTestAutoscaler(t, testOptions(), 1)

	a.SetObserver(&fakeObserver{err: errors.New("github unreachable")})

	a.OnQueued(1)
	a.OnQueued(2)
	a.reconcile(t.Context())

	if got, ok := fb.last(); !ok || got != 2 {
		t.Fatalf("want webhook-driven scale to 2 despite observer failure, got %v (ok=%v)", got, ok)
	}
}

// Without a credential the autoscaler must behave exactly as it did before.
func TestNoObserverLeavesBehaviourUnchanged(t *testing.T) {
	opts := testOptions()
	a, fb, clock := newTestAutoscaler(t, opts, 1)

	*clock = clock.Add(opts.RunnerGrace * 10)
	a.reconcile(t.Context())

	if n := fb.count(); n != 0 {
		t.Fatalf("want no recycle without an observer, got %d calls", n)
	}
	if s := a.Stats(); s.LiveRunners != -1 {
		t.Fatalf("want liveRunners unknown without an observer, got %d", s.LiveRunners)
	}
}
