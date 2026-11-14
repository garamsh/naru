package linux

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/service"
)

// Sources hands out the writer that tags what a service writes with its name.
type Sources interface {
	Source(name string) io.Writer
}

// Launcher starts services as children of the process, inside their cgroup,
// with a pipe as output that it copies to the console, tagged with the
// service's name, and the lines appended to the files the service names as LogFiles. Their exits
// are reported by the Reaper.
type Launcher struct {
	reaper  *Reaper
	console Sources
}

var _ service.Launcher = (*Launcher)(nil)

// NewLauncher returns a Launcher whose children reaper collects and whose
// children's output goes to console.
func NewLauncher(reaper *Reaper, console Sources) *Launcher {
	return &Launcher{reaper: reaper, console: console}
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

	// The service writes to a pipe that init copies to the console, which keeps the lines
	// with the service's name.
	out, in, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer in.Close()
	cmd := exec.Command(svc.Binary, svc.Args...)
	cmd.Env = environ(svc.Env)
	cmd.Stdout, cmd.Stderr = in, in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: int(dir.Fd())}

	// A log file is followed from where it ends now, so a restart does not repeat what it kept.
	offsets := map[string]int64{}
	for _, path := range svc.LogFiles {
		offsets[path] = size(path)
	}

	exited, err := l.reaper.Spawn(func() (int, error) {
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	})
	if err != nil {
		out.Close()
		return nil, err
	}
	stop := make(chan struct{})
	var tails sync.WaitGroup
	for path, offset := range offsets {
		tails.Add(1)
		go func() {
			defer tails.Done()
			follow(path, offset, l.console.Source(svc.Name), stop)
		}()
	}
	go func() {
		defer out.Close()
		_, _ = io.Copy(l.console.Source(svc.Name), out)
		close(stop)
		tails.Wait()
	}()
	return &process{proc: cmd.Process, pid: cmd.Process.Pid, exited: exited}, nil
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
	pid    int
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

func (p *process) PID() int { return p.pid }

func (p *process) Signal(sig syscall.Signal) error {
	return p.proc.Signal(sig)
}
