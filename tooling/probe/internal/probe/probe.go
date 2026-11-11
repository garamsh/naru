// Package probe is the check the QEMU tests run inside a Node: that a Machine
// outlives mvmd. It starts a Machine, kills mvmd, waits for init to restart
// it, and reports whether the Machine's supervisor is the same process and
// mvmd reports the Machine still running. Each step prints one line,
// "PROBE <check> ok|fail <detail>", for the host to read from the console.
package probe

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/garamsh/naru/tooling/probe/internal/proc"
)

// Runtime is mvmd's runtime API as the probe uses it.
type Runtime interface {
	// Ready returns nil once mvmd answers a call on its socket; each call connects afresh.
	Ready(ctx context.Context) error
	// Arm arms mvmd's fencing deadline, which starting a Machine needs.
	Arm(ctx context.Context) error
	Pull(ctx context.Context, repository, digest string) error
	CreateAndStart(ctx context.Context, id, imageDigest string) error
	// States is the state Inspect and List each report for the Machine.
	States(ctx context.Context, id string) (inspected, listed string, err error)
}

// Host is the Node's processes.
type Host interface {
	Find(cgroupDir, comm string) (proc.Process, error)
	Alive(proc.Process) bool
	Kill(pid int) error
}

// Config says what to run and where.
type Config struct {
	Repository, Digest string
	// MachineID names the Machine.
	MachineID string
	// MvmdCgroup and SupervisorCgroup are the cgroup directories init gives mvmd and
	// mvmd gives its supervisors.
	MvmdCgroup, SupervisorCgroup string
	// Running is the state name that means a Machine is running.
	Running string
	// Poll is the wait between looks at a condition.
	Poll time.Duration
}

// Run does the checks in order and stops at the first that fails. It reports
// "PROBE finished" last, on success and on failure, so that a host reading the
// console can tell a probe that ended from one that never ran. It returns
// whether every check passed.
func Run(ctx context.Context, cfg Config, rt Runtime, host Host, out io.Writer) bool {
	r := &run{ctx: ctx, cfg: cfg, rt: rt, host: host, out: out}
	ok := r.checks()
	_, _ = fmt.Fprintln(out, "PROBE finished")
	return ok
}

type run struct {
	ctx  context.Context
	cfg  Config
	rt   Runtime
	host Host
	out  io.Writer
}

func (r *run) report(check string, err error, detail string, args ...any) bool {
	if err != nil {
		_, _ = fmt.Fprintf(r.out, "PROBE %s fail %s\n", check, oneLine(err.Error()))
		return false
	}
	_, _ = fmt.Fprintf(r.out, "PROBE %s ok %s\n", check, fmt.Sprintf(detail, args...))
	return true
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// until calls f every Poll until it returns nil, and returns its last error
// when ctx ends first.
func (r *run) until(f func() error) error {
	for {
		err := f()
		if err == nil {
			return nil
		}
		select {
		case <-r.ctx.Done():
			return fmt.Errorf("%w (last: %v)", r.ctx.Err(), err)
		case <-time.After(r.cfg.Poll):
		}
	}
}

func (r *run) checks() bool {
	if !r.report("mvmd-ready", r.until(func() error { return r.rt.Ready(r.ctx) }), "the runtime API answers") {
		return false
	}
	if !r.report("fencing-armed", r.rt.Arm(r.ctx), "the deadline is armed") {
		return false
	}
	// The registry is a service of the Node that starts beside mvmd, so the pull waits for it.
	pulled := r.until(func() error { return r.rt.Pull(r.ctx, r.cfg.Repository, r.cfg.Digest) })
	if !r.report("image-pulled", pulled, "%s@%s", r.cfg.Repository, r.cfg.Digest) {
		return false
	}
	if !r.report("machine-started", r.rt.CreateAndStart(r.ctx, r.cfg.MachineID, r.cfg.Digest), "machine=%s", r.cfg.MachineID) {
		return false
	}
	if !r.report("machine-running", r.until(func() error { return r.wantRunning() }), "machine=%s", r.cfg.MachineID) {
		return false
	}
	supervisor, err := r.host.Find(r.cfg.SupervisorCgroup, "mvmd-supervisor")
	if !r.report("supervisor-found", err, "pid=%d", supervisor.PID) {
		return false
	}
	mvmd, err := r.mvmd()
	if !r.report("mvmd-found", err, "pid=%d", mvmd.PID) {
		return false
	}
	if !r.report("mvmd-killed", r.host.Kill(mvmd.PID), "pid=%d signal=SIGKILL", mvmd.PID) {
		return false
	}
	var restarted proc.Process
	err = r.until(func() error {
		p, err := r.mvmd()
		if err != nil {
			return err
		}
		if p.PID == mvmd.PID && p.Start == mvmd.Start {
			return fmt.Errorf("mvmd pid %d has not been replaced", p.PID)
		}
		restarted = p
		return r.rt.Ready(r.ctx)
	})
	if !r.report("mvmd-restarted", err, "old=%d new=%d, the socket answers", mvmd.PID, restarted.PID) {
		return false
	}
	alive := r.host.Alive(supervisor)
	if !alive {
		err = fmt.Errorf("supervisor pid %d is not the process it was", supervisor.PID)
	} else {
		err = nil
	}
	if !r.report("supervisor-alive", err, "pid=%d is the same process", supervisor.PID) {
		return false
	}
	again, err := r.host.Find(r.cfg.SupervisorCgroup, "mvmd-supervisor")
	if err == nil && again.PID != supervisor.PID {
		err = fmt.Errorf("the supervisor is pid %d, was %d", again.PID, supervisor.PID)
	}
	if !r.report("supervisor-same-pid", err, "pid=%d before and after", supervisor.PID) {
		return false
	}
	return r.report("machine-still-running", r.wantRunning(), "machine=%s, Inspect and List agree", r.cfg.MachineID)
}

func (r *run) mvmd() (proc.Process, error) {
	return r.host.Find(r.cfg.MvmdCgroup, "mvmd")
}

func (r *run) wantRunning() error {
	inspected, listed, err := r.rt.States(r.ctx, r.cfg.MachineID)
	if err != nil {
		return err
	}
	if inspected != r.cfg.Running || listed != r.cfg.Running {
		return fmt.Errorf("Inspect says %s and List says %s, want %s", inspected, listed, r.cfg.Running)
	}
	return nil
}
