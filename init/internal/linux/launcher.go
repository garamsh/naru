package linux

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/service"
)

// Launcher starts services as children of the process, inside their cgroup,
// with the console as output. Their exits are reported by the Reaper.
type Launcher struct {
	reaper *Reaper
}

var _ service.Launcher = (*Launcher)(nil)

// NewLauncher returns a Launcher whose children reaper collects.
func NewLauncher(reaper *Reaper) *Launcher {
	return &Launcher{reaper: reaper}
}

// Launch starts svc with exactly the environment its configuration names.
// The child is created in the cgroup directly (CLONE_INTO_CGROUP), so it is
// never outside it.
func (l *Launcher) Launch(svc config.Service, cgroup string) (service.Process, error) {
	dir, err := os.Open(cgroup)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	cmd := exec.Command(svc.Binary, svc.Args...)
	cmd.Env = environ(svc.Env)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: int(dir.Fd())}

	exited, err := l.reaper.Spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		return nil, err
	}
	return &process{proc: cmd.Process, exited: exited}, nil
}

func environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

type process struct {
	proc   *os.Process
	exited <-chan syscall.WaitStatus
}

// Wait returns once the reaper has collected the process.
func (p *process) Wait() error {
	ws := <-p.exited
	_ = p.proc.Release()
	switch {
	case ws.Exited() && ws.ExitStatus() == 0:
		return nil
	case ws.Exited():
		return fmt.Errorf("exit status %d", ws.ExitStatus())
	default:
		return fmt.Errorf("signal: %s", ws.Signal())
	}
}

func (p *process) Signal(sig syscall.Signal) error {
	return p.proc.Signal(sig)
}
