package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/init/config"
)

// credential returns what the child process is started with to run as user, or nil for root.
// The supplementary groups are exactly the ones the user names: a service never keeps init's.
func credential(user *config.User) *syscall.Credential {
	if user == nil {
		return nil
	}
	return &syscall.Credential{Uid: user.UID, Gid: user.GID, Groups: append([]uint32{}, user.Groups...)}
}

// procAttr returns how svc's processes start: in their own session, created in the cgroup open as
// cgroupFD, as svc's user, and with CAP_NET_BIND_SERVICE as the one ambient capability when it
// binds a low port as a user other than root.
func procAttr(svc config.Service, cgroupFD int) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: cgroupFD, Credential: credential(svc.User)}
	if svc.BindLowPorts && svc.User != nil {
		attr.AmbientCaps = []uintptr{unix.CAP_NET_BIND_SERVICE}
	}
	return attr
}

// limitOpenFiles sets the open-file limit of the process pid, soft and hard, to n; 0 leaves the
// limit it inherited from init. It is set once the process has started, since a process cannot be
// started with a limit of its own, so a descriptor the process opens first counts under the old.
func limitOpenFiles(pid int, n uint64) error {
	if n == 0 {
		return nil
	}
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: n, Max: n}, nil); err != nil {
		return fmt.Errorf("limit the open files of %d to %d: %w", pid, n, err)
	}
	return nil
}

// prepareDirs makes each directory with its parents when it is missing, then sets its owner and
// mode, so a directory a service keeps its keys in is the service's and nobody else's whatever
// it was before. A path that is not a directory, a symbolic link among them, is refused.
func prepareDirs(dirs []config.Dir) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d.Path, 0o755); err != nil {
			return fmt.Errorf("make %s: %w", d.Path, err)
		}
		info, err := os.Lstat(d.Path)
		if err != nil {
			return fmt.Errorf("make %s: %w", d.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("make %s: it is not a directory", d.Path)
		}
		if err := os.Lchown(d.Path, int(d.UID), int(d.GID)); err != nil {
			return fmt.Errorf("own %s: %w", d.Path, err)
		}
		if err := os.Chmod(d.Path, d.Perm()); err != nil {
			return fmt.Errorf("set the mode of %s: %w", d.Path, err)
		}
	}
	return nil
}

// setupTimeout is how long one setup step may run before it is killed: a step that waits for a
// peer that is not there would otherwise keep its service from ever starting or stopping.
const setupTimeout = 5 * time.Minute

// runSetup runs the setups of svc in order, each to completion unless a path it names exists. newCmd returns
// the command of the service's binary with the arguments given and everything else (the
// environment, the user, the cgroup) that the service's own start has; its output goes to out.
// A step ends when ctx does, or after the bound, with the cgroup at cgroup killed.
func (l *Launcher) runSetup(ctx context.Context, svc config.Service, cgroup string, newCmd func(args []string) *exec.Cmd, out io.Writer) error {
	for _, st := range svc.Setup {
		if err := l.runStep(ctx, svc.Name, cgroup, st, newCmd, out); err != nil {
			return err
		}
	}
	return nil
}

// runStep runs one setup, unless a path it names exists, and each setup that follows it sees what it made.
func (l *Launcher) runStep(ctx context.Context, name, cgroup string, st config.Setup, newCmd func(args []string) *exec.Cmd, out io.Writer) error {
	for _, u := range st.Unless {
		if _, err := os.Lstat(u); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("setup of %s: %w", name, err)
		}
	}
	bound := l.setupBound
	if bound == 0 {
		bound = setupTimeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	ws, err := l.reaper.Exec(stepCtx, newCmd(st.Args), out, func() { killCgroup(cgroup) })
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("setup of %s: killed after %s", name, bound)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("setup of %s: killed, the service was stopped", name)
	case err != nil:
		return fmt.Errorf("setup of %s: %w", name, err)
	}
	if err := statusError(ws); err != nil {
		return fmt.Errorf("setup of %s: %w", name, err)
	}
	return nil
}

// killCgroup kills every process of the cgroup directory dir, and of its descendants. It is best
// effort: the process it is called for is killed on its own as well.
func killCgroup(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("1")
}
