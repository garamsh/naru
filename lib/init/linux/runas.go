package linux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

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

// runSetup runs the setups of svc in order, each to completion unless a path it names exists. newCmd returns
// the command of the service's binary with the arguments given and everything else (the
// environment, the user, the cgroup) that the service's own start has; its output goes to out.
func (l *Launcher) runSetup(svc config.Service, newCmd func(args []string) *exec.Cmd, out io.Writer) error {
	for _, st := range svc.Setup {
		if err := l.runStep(svc.Name, st, newCmd, out); err != nil {
			return err
		}
	}
	return nil
}

// runStep runs one setup, unless a path it names exists, and each setup that follows it sees what it made.
func (l *Launcher) runStep(name string, st config.Setup, newCmd func(args []string) *exec.Cmd, out io.Writer) error {
	for _, u := range st.Unless {
		if _, err := os.Lstat(u); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("setup of %s: %w", name, err)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd := newCmd(st.Args)
	cmd.Stdout, cmd.Stderr = w, w
	exited, err := l.reaper.Spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	w.Close()
	if err != nil {
		return fmt.Errorf("setup of %s: %w", name, err)
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, r)
	}()
	ws := <-exited
	_ = cmd.Process.Release()
	<-copied
	switch {
	case ws.Exited() && ws.ExitStatus() == 0:
		return nil
	case ws.Exited():
		return fmt.Errorf("setup of %s: exit status %d", name, ws.ExitStatus())
	default:
		return fmt.Errorf("setup of %s: signal: %s", name, ws.Signal())
	}
}
