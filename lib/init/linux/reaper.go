package linux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// Reaper collects every child of the process, orphans reparented to PID 1
// included, and reports the exit of the ones started through Spawn.
type Reaper struct {
	// mu is held across a start and its registration, and across a reap, so a
	// child cannot be reaped between the two.
	mu       sync.Mutex
	watchers map[int]chan syscall.WaitStatus
}

// NewReaper returns a Reaper that is not yet reaping; run Run.
func NewReaper() *Reaper {
	return &Reaper{watchers: map[int]chan syscall.WaitStatus{}}
}

// Spawn calls start, which must start one child and return its pid, and
// returns a channel that receives the child's wait status once it exits.
func (r *Reaper) Spawn(start func() (pid int, err error)) (<-chan syscall.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := start()
	if err != nil {
		return nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	r.watchers[pid] = ch
	return ch, nil
}

// Exec starts cmd through Spawn, copies its output (both streams) to out, and returns the wait
// status once the Reaper has collected it. cmd.Wait would wait on the pid itself, which the Reaper
// may have reaped first and then report as ECHILD, so no child of init is run any other way. When
// ctx ends first Exec kills the process, and also calls killAlso (when not nil) for what the process
// left behind, since a descendant that holds the output open would keep Exec from returning; the
// error is then ctx's. cmd must not have Stdout, Stderr or Stdin set.
func (r *Reaper) Exec(ctx context.Context, cmd *exec.Cmd, out io.Writer, killAlso func()) (syscall.WaitStatus, error) {
	rd, wr, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer rd.Close()
	cmd.Stdout, cmd.Stderr = wr, wr
	exited, err := r.Spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	wr.Close()
	if err != nil {
		return 0, err
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, rd)
	}()
	var killed atomic.Bool
	stop := context.AfterFunc(ctx, func() {
		killed.Store(true)
		_ = cmd.Process.Kill()
		if killAlso != nil {
			killAlso()
		}
	})
	ws := <-exited
	stop()
	_ = cmd.Process.Release()
	<-copied
	if killed.Load() {
		return ws, ctx.Err()
	}
	return ws, nil
}

// Output runs the program through Exec and returns its combined output. A program that does not
// exit with status 0 is an error.
func (r *Reaper) Output(name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	ws, err := r.Exec(context.Background(), exec.Command(name, args...), &out, nil)
	if err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), statusError(ws)
}

// statusError returns nil for a clean exit and otherwise says how the process ended.
func statusError(ws syscall.WaitStatus) error {
	switch {
	case ws.Exited() && ws.ExitStatus() == 0:
		return nil
	case ws.Exited():
		return fmt.Errorf("exit status %d", ws.ExitStatus())
	default:
		return fmt.Errorf("signal: %s", ws.Signal())
	}
}

// Run reaps on every SIGCHLD until ctx ends.
func (r *Reaper) Run(ctx context.Context) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	defer signal.Stop(sigs)
	for {
		r.reapAll()
		select {
		case <-sigs:
		case <-ctx.Done():
			return
		}
	}
}

// reapAll waits for every child that has already exited.
func (r *Reaper) reapAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil || pid <= 0:
			return
		}
		if ch, ok := r.watchers[pid]; ok {
			ch <- ws
			delete(r.watchers, pid)
		}
	}
}
