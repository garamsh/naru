// Command naru-probe is the service the QEMU tests configure to check, from
// inside a Node, that a Machine outlives mvmd. It prints "PROBE ..." lines to
// the console (see internal/probe) and then runs until stopped, so that init
// does not restart it and run the checks again.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/probe/internal/probe"
	"github.com/garamsh/naru/tooling/probe/internal/proc"
)

func main() {
	var (
		socket  = flag.String("socket", "/run/mvmd.sock", "mvmd's runtime API socket")
		repo    = flag.String("image-repository", "", "repository of the image to pull")
		digest  = flag.String("image-digest", "", "digest of the image to pull")
		mvmdCg  = flag.String("mvmd-cgroup", "/sys/fs/cgroup/naru/mvmd", "cgroup init gives mvmd")
		supCg   = flag.String("supervisor-cgroup", "/sys/fs/cgroup/naru/detached", "cgroup mvmd gives its supervisors")
		timeout = flag.Duration("timeout", 5*time.Minute, "how long all checks may take")
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	probe.Run(runCtx, probe.Config{
		Repository: *repo, Digest: *digest, MachineID: "probe",
		MvmdCgroup: *mvmdCg, SupervisorCgroup: *supCg,
		Running: probe.RunningState, Poll: 200 * time.Millisecond,
	}, probe.Mvmd{Socket: *socket, MemoryBytes: 256 << 20, DiskBytes: 256 << 20},
		probe.System{Reader: proc.Reader{Proc: "/proc"}}, os.Stdout)
	<-ctx.Done()
}
