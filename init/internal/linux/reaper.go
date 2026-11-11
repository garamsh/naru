package linux

import (
	"context"
	"os"
	"os/signal"
	"sync"
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
