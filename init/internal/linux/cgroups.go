// Package linux implements init's dependencies on the running kernel: cgroup
// v2, child reaping, process launch, mounts, disks, sysctls and power.
package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/service"
)

const (
	// controllers are enabled at the root and in naru/ so a service can create its own limits (mvmd ADR 0015); cpuset and hugetlb are the kubelet's, which refuses a cgroup without them (ADR 0031).
	controllers = "+cpu +cpuset +hugetlb +memory +pids"
	naruGroup   = "naru"

	killTimeout   = 5 * time.Second
	removeRetries = 20
)

// CgroupTree is the cgroup v2 hierarchy services run in: <root>/naru/<service>
// for each service, and <root>/naru/detached for processes a service places
// outside its own.
type CgroupTree struct {
	root string
}

var _ service.Cgroups = (*CgroupTree)(nil)

// NewCgroupTree returns the tree under the cgroup v2 directory root.
func NewCgroupTree(root string) *CgroupTree {
	return &CgroupTree{root: root}
}

// cpu, cpuset, hugetlb, memory and pids controllers at the root, in naru/ and in
// cpu, memory and pids controllers at the root, in naru/ and in
// naru/kubernetes, which holds no process: the kubelet makes its pod cgroups
// there (ADR 0031). root must hold no process other than the root of a real
// cgroup hierarchy.
func (t *CgroupTree) Prepare() error {
	naru := filepath.Join(t.root, naruGroup)
	if err := os.MkdirAll(filepath.Join(naru, config.DetachedName), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", naru, err)
	}
	delegated := filepath.Join(naru, config.DelegatedName)
	if err := os.MkdirAll(delegated, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", delegated, err)
	}
	for _, dir := range []string{t.root, naru, delegated} {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(controllers), 0o644); err != nil {
			return fmt.Errorf("enable %q in %s: %w", controllers, dir, err)
		}
	}
	return nil
}

// Leaf returns the cgroup of the service name, creating it if absent.
func (t *CgroupTree) Leaf(name string) (service.Leaf, error) {
	path := filepath.Join(t.root, naruGroup, name)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return &leaf{path: path}, nil
}

type leaf struct {
	path string
}

func (l *leaf) Path() string { return l.path }

// Kill writes cgroup.kill, which kills the cgroup's processes and those of
// its descendants and no others, then waits for cgroup.procs to empty.
func (l *leaf) Kill() error {
	if err := os.WriteFile(filepath.Join(l.path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("write cgroup.kill: %w", err)
	}
	deadline := time.Now().Add(killTimeout)
	for {
		procs, err := os.ReadFile(filepath.Join(l.path, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("read cgroup.procs: %w", err)
		}
		if strings.TrimSpace(string(procs)) == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still holds processes %s after %s", l.path, strings.Fields(string(procs)), killTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Remove deletes the cgroup. The kernel releases a cgroup of exited
// processes a moment after the last exit, so a busy cgroup is retried briefly.
func (l *leaf) Remove() error {
	var err error
	for range removeRetries {
		if err = os.Remove(l.path); err == nil || errors.Is(err, os.ErrNotExist) || !errors.Is(err, syscall.EBUSY) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}
