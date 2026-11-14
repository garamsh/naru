package service_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/service"
)

// recv returns the next value on ch, and fails the test if none arrives.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the manager")
		panic("unreachable")
	}
}

// fakeClock hands every After to the test, which decides when it fires.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *timer
}

type timer struct {
	d  time.Duration
	ch chan time.Time
}

func (t *timer) fire() { t.ch <- time.Time{} }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1000, 0), timers: make(chan *timer, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	tm := &timer{d: d, ch: make(chan time.Time, 1)}
	c.timers <- tm
	return tm.ch
}

// fakeProc is a process that exits when the test says so, or on SIGTERM.
type fakeProc struct {
	exitOnTerm bool
	pid        int

	mu      sync.Mutex
	signals []syscall.Signal
	once    sync.Once
	done    chan struct{}
	err     error
}

func (p *fakeProc) exit(err error) {
	p.once.Do(func() { p.err = err; close(p.done) })
}

func (p *fakeProc) PID() int { return p.pid }

func (p *fakeProc) Wait() error {
	<-p.done
	return p.err
}

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if sig == syscall.SIGTERM && p.exitOnTerm {
		p.exit(nil)
	}
	return nil
}

func (p *fakeProc) received() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]syscall.Signal(nil), p.signals...)
}

// launch records where each process was started.
type launch struct {
	svc    config.Service
	cgroup string
	proc   *fakeProc
}

type fakeLauncher struct {
	pids       atomic.Int32
	exitOnTerm bool
	failures   chan error // a value queued here fails the next Launch
	launched   chan launch
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{exitOnTerm: true, failures: make(chan error, 8), launched: make(chan launch, 64)}
}

func (l *fakeLauncher) Launch(svc config.Service, cgroup string) (service.Process, error) {
	select {
	case err := <-l.failures:
		return nil, err
	default:
	}
	p := &fakeProc{exitOnTerm: l.exitOnTerm, pid: 100 + int(l.pids.Add(1)), done: make(chan struct{})}
	l.launched <- launch{svc: svc, cgroup: cgroup, proc: p}
	return p, nil
}

var errExit = errors.New("exit status 1")

type fakeLeaf struct {
	path string

	mu      sync.Mutex
	kills   int
	removes int
}

func (l *fakeLeaf) Path() string { return l.path }

func (l *fakeLeaf) Kill() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kills++
	return nil
}

func (l *fakeLeaf) Remove() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.removes++
	return nil
}

func (l *fakeLeaf) counts() (kills, removes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.kills, l.removes
}

type fakeCgroups struct {
	mu     sync.Mutex
	leaves map[string]*fakeLeaf
}

func (c *fakeCgroups) Prepare() error { return nil }

func (c *fakeCgroups) Leaf(name string) (service.Leaf, error) {
	return c.leaf(name), nil
}

func (c *fakeCgroups) leaf(name string) *fakeLeaf {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaves == nil {
		c.leaves = map[string]*fakeLeaf{}
	}
	if c.leaves[name] == nil {
		c.leaves[name] = &fakeLeaf{path: "/cgroup/naru/" + name}
	}
	return c.leaves[name]
}
