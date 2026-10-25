package fly

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Machines is the subset of Client a Pool drives.
type Machines interface {
	ListMachines(ctx context.Context) ([]Machine, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
}

// BusyRunners returns the names of runners currently executing a job.
type BusyRunners func(ctx context.Context) ([]string, error)

// Pool scales a fixed set of pre-created Machines by starting and stopping them.
//
// Each Machine runs one ephemeral runner with restart policy "no": the runner
// takes a job, exits, and the Machine stops itself. Fly rebuilds the root
// filesystem from the image on every start, so a restarted Machine is a clean
// runner. The pool's size is the number of Machines in the app (`fly scale
// count`); MAX_RUNNERS beyond that cannot be met.
//
// Unlike Railway, which picks the replica to terminate, the pool chooses which
// Machine to stop. Runners register as <prefix>-<hostname>, and a Machine's
// hostname is its ID, so with Busy set a scale-down only ever stops Machines
// whose runner is idle.
type Pool struct {
	Machines Machines
	// Busy is optional. Without it, a scale-down stops arbitrary active
	// Machines, which is only as safe as the scaler's own scale-down gates.
	Busy   BusyRunners
	Logger *slog.Logger
}

// Replicas reports how many Machines are running or starting. Machines stop on
// their own when their runner finishes a job, so this is read fresh rather than
// remembered.
func (p *Pool) Replicas(ctx context.Context) (int, error) {
	ms, err := p.Machines.ListMachines(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range ms {
		if m.Active() {
			n++
		}
	}
	return n, nil
}

// SetReplicas starts or stops Machines until n are active.
func (p *Pool) SetReplicas(ctx context.Context, n int) error {
	ms, err := p.Machines.ListMachines(ctx)
	if err != nil {
		return fmt.Errorf("list machines: %w", err)
	}
	// Deterministic order, so repeated reconciles reach for the same Machines.
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })

	var active, startable []Machine
	for _, m := range ms {
		switch {
		case m.Active():
			active = append(active, m)
		case m.Startable():
			startable = append(startable, m)
		}
	}

	switch {
	case len(active) < n:
		return p.start(ctx, n-len(active), startable, len(ms))
	case len(active) > n:
		return p.stop(ctx, len(active)-n, active)
	}
	return nil
}

func (p *Pool) start(ctx context.Context, want int, startable []Machine, total int) error {
	if len(startable) < want {
		// Machines mid-stop become startable on a later reconcile; a pool
		// smaller than MAX_RUNNERS never does.
		p.Logger.Warn("not enough stopped machines to start",
			"want", want, "startable", len(startable), "poolSize", total)
	}

	var errs []error
	started := 0
	for _, m := range startable {
		if started == want {
			break
		}
		if err := p.Machines.Start(ctx, m.ID); err != nil {
			// One Machine failing to start (host capacity, say) should not
			// stop the others from covering for it.
			errs = append(errs, fmt.Errorf("start %s: %w", m.ID, err))
			continue
		}
		started++
		p.Logger.Info("started machine", "machine", m.ID, "region", m.Region)
	}
	if started < want && len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (p *Pool) stop(ctx context.Context, excess int, active []Machine) error {
	busy := map[string]bool{}
	if p.Busy != nil {
		names, err := p.Busy(ctx)
		if err != nil {
			// Guessing could stop a Machine mid-job; wait for the next reconcile.
			return fmt.Errorf("list busy runners: %w", err)
		}
		for _, m := range active {
			for _, name := range names {
				if strings.HasSuffix(name, m.ID) {
					busy[m.ID] = true
				}
			}
		}
	}

	var errs []error
	stopped := 0
	for _, m := range active {
		if stopped == excess {
			break
		}
		if busy[m.ID] {
			continue
		}
		if err := p.Machines.Stop(ctx, m.ID); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", m.ID, err))
			continue
		}
		stopped++
		p.Logger.Info("stopped idle machine", "machine", m.ID)
	}
	if stopped < excess && len(errs) == 0 {
		// The rest are mid-job; each stops itself when its runner exits.
		p.Logger.Info("leaving busy machines to finish their jobs",
			"excess", excess-stopped, "busy", len(busy))
	}
	return errors.Join(errs...)
}
