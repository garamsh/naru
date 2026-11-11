// Package service starts the configured services, each in a cgroup of its
// own, restarts one that exits after a backoff, and stops them.
package service

import (
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/config"
)

// stopGrace is how long a service has to exit on SIGTERM before its cgroup is killed.
const stopGrace = 5 * time.Second

// Manager supervises the services.
type Manager interface {
	// Prepare builds the cgroup hierarchy. It runs once, before any Start.
	Prepare() error
	// Start runs svc in its own cgroup and restarts it whenever it exits, until Stop.
	Start(svc config.Service) error
	// Stop ends the named service and kills every process left in its cgroup.
	// It does nothing for a name that is not running.
	Stop(name string)
	// StopAll stops every service concurrently.
	StopAll()
}

// NewManager returns a Manager that runs services through launcher in leaves from cgroups.
func NewManager(cgroups Cgroups, launcher Launcher, clock Clock, log *slog.Logger) Manager {
	return &manager{cgroups: cgroups, launcher: launcher, clock: clock, log: log, running: map[string]*supervised{}}
}

type manager struct {
	cgroups  Cgroups
	launcher Launcher
	clock    Clock
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]*supervised
}

// supervised is one service's supervising goroutine.
type supervised struct {
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (m *manager) Prepare() error {
	return m.cgroups.Prepare()
}

func (m *manager) Start(svc config.Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.running[svc.Name]; ok {
		return fmt.Errorf("service %q is already running", svc.Name)
	}
	leaf, err := m.cgroups.Leaf(svc.Name)
	if err != nil {
		return fmt.Errorf("cgroup of service %q: %w", svc.Name, err)
	}
	s := &supervised{stop: make(chan struct{}), done: make(chan struct{})}
	m.running[svc.Name] = s
	go m.supervise(s, svc, leaf)
	return nil
}

func (m *manager) Stop(name string) {
	m.mu.Lock()
	s, ok := m.running[name]
	delete(m.running, name)
	m.mu.Unlock()
	if !ok {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

func (m *manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.running))
	for name := range m.running {
		names = append(names, name)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Stop(name)
		}()
	}
	wg.Wait()
}

// supervise runs svc until s.stop closes. After every exit it kills what the
// process left in the leaf, so a restart starts from an empty cgroup.
func (m *manager) supervise(s *supervised, svc config.Service, leaf Leaf) {
	defer close(s.done)
	log := m.log.With("service", svc.Name)
	var backoff Backoff
	for {
		started := m.clock.Now()
		proc, err := m.launcher.Launch(svc, leaf.Path())
		if err != nil {
			log.Error("start failed", "err", err)
		} else {
			log.Info("started", "cgroup", leaf.Path())
			exited := make(chan error, 1)
			go func() { exited <- proc.Wait() }()
			select {
			case err := <-exited:
				log.Warn("exited", "err", err)
			case <-s.stop:
				m.terminate(log, proc, exited)
				m.end(log, leaf)
				return
			}
		}
		if err := leaf.Kill(); err != nil {
			log.Error("kill leftovers failed", "err", err)
		}
		delay := backoff.Next(m.clock.Now().Sub(started))
		log.Info("restarting", "delay", delay)
		select {
		case <-m.clock.After(delay):
		case <-s.stop:
			m.end(log, leaf)
			return
		}
	}
}

// terminate asks proc to exit, and gives it stopGrace to do so.
func (m *manager) terminate(log *slog.Logger, proc Process, exited <-chan error) {
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Warn("SIGTERM failed", "err", err)
	}
	select {
	case <-exited:
	case <-m.clock.After(stopGrace):
		log.Warn("did not exit on SIGTERM, killing its cgroup")
	}
}

// end kills every process left in the leaf and removes it.
func (m *manager) end(log *slog.Logger, leaf Leaf) {
	if err := leaf.Kill(); err != nil {
		log.Error("kill failed", "err", err)
	}
	if err := leaf.Remove(); err != nil {
		log.Error("remove cgroup failed", "err", err)
	}
	log.Info("stopped")
}
