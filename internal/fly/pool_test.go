package fly

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// fakeMachines is an in-memory app whose Start and Stop flip Machine state.
type fakeMachines struct {
	mu       sync.Mutex
	machines map[string]string // id -> state
	failOn   map[string]error
	started  []string
	stopped  []string
	listErr  error
}

func newFakeMachines(states map[string]string) *fakeMachines {
	return &fakeMachines{machines: states, failOn: map[string]error{}}
}

func (f *fakeMachines) ListMachines(context.Context) ([]Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []Machine
	for id, st := range f.machines {
		out = append(out, Machine{ID: id, State: st})
	}
	// Map order is random; the pool must not depend on list order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (f *fakeMachines) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[id]; err != nil {
		return err
	}
	f.machines[id] = StateStarted
	f.started = append(f.started, id)
	return nil
}

func (f *fakeMachines) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[id]; err != nil {
		return err
	}
	f.machines[id] = StateStopping
	f.stopped = append(f.stopped, id)
	return nil
}

func busyNames(names ...string) BusyRunners {
	return func(context.Context) ([]string, error) { return names, nil }
}

func TestPoolStartsStoppedMachinesToMeetDemand(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarted, "b": StateStopped, "c": StateStopped, "d": StateStopped})
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 3); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if !reflect.DeepEqual(m.started, []string{"b", "c"}) {
		t.Errorf("started = %v, want [b c]", m.started)
	}
	if n, _ := p.Replicas(t.Context()); n != 3 {
		t.Errorf("Replicas = %d, want 3", n)
	}
}

func TestPoolCountsStartingMachinesAsCapacity(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarting, "b": StateStopped})
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 1); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if len(m.started) != 0 || len(m.stopped) != 0 {
		t.Errorf("want no-op, started=%v stopped=%v", m.started, m.stopped)
	}
}

func TestPoolStopsOnlyIdleMachines(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarted, "b": StateStarted, "c": StateStarted})
	// Runners register as <prefix>-<machine id>.
	p := &Pool{Machines: m, Busy: busyNames("fly-a", "fly-c"), Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 0); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if !reflect.DeepEqual(m.stopped, []string{"b"}) {
		t.Errorf("stopped = %v, want only the idle machine [b]", m.stopped)
	}
}

func TestPoolStopsOnlyTheExcess(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarted, "b": StateStarted, "c": StateStarted})
	p := &Pool{Machines: m, Busy: busyNames(), Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 2); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if len(m.stopped) != 1 {
		t.Errorf("stopped = %v, want exactly one", m.stopped)
	}
}

func TestPoolWithoutBusyStopsArbitraryMachines(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarted, "b": StateStarted})
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 0); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if len(m.stopped) != 2 {
		t.Errorf("stopped = %v, want both", m.stopped)
	}
}

func TestPoolRefusesToStopWhenBusyIsUnknown(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStarted})
	p := &Pool{
		Machines: m,
		Busy:     func(context.Context) ([]string, error) { return nil, errors.New("github down") },
		Logger:   discardLogger(),
	}

	if err := p.SetReplicas(t.Context(), 0); err == nil {
		t.Fatal("want error so the scaler retries rather than guessing")
	}
	if len(m.stopped) != 0 {
		t.Errorf("stopped = %v, want none", m.stopped)
	}
}

func TestPoolSkipsMachinesThatFailToStart(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStopped, "b": StateStopped})
	m.failOn["a"] = errors.New("no capacity")
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 1); err != nil {
		t.Fatalf("want b to cover for a, got %v", err)
	}
	if !reflect.DeepEqual(m.started, []string{"b"}) {
		t.Errorf("started = %v, want [b]", m.started)
	}
}

func TestPoolReportsStartFailureWhenShort(t *testing.T) {
	m := newFakeMachines(map[string]string{"a": StateStopped})
	m.failOn["a"] = errors.New("no capacity")
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 1); err == nil {
		t.Fatal("want error when no machine could be started")
	}
}

func TestPoolSmallerThanDemandIsNotAnError(t *testing.T) {
	// Machines mid-stop and a pool smaller than MAX_RUNNERS are both
	// conditions the next reconcile handles; neither should fail the call.
	m := newFakeMachines(map[string]string{"a": StateStopped, "b": StateStopping})
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 3); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if !reflect.DeepEqual(m.started, []string{"a"}) {
		t.Errorf("started = %v, want [a]", m.started)
	}
}

func TestPoolPropagatesListErrors(t *testing.T) {
	m := newFakeMachines(nil)
	m.listErr = errors.New("boom")
	p := &Pool{Machines: m, Logger: discardLogger()}

	if err := p.SetReplicas(t.Context(), 1); err == nil {
		t.Fatal("want SetReplicas error")
	}
	if _, err := p.Replicas(t.Context()); err == nil {
		t.Fatal("want Replicas error")
	}
}
