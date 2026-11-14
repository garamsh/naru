package loginprobe_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/loginprobe"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// node is a root file system and a process file system on disk, which the probe reads as it reads a
// Node's.
type node struct{ root, proc string }

func newNode(t *testing.T) node {
	t.Helper()
	n := node{root: filepath.Join(t.TempDir(), "root"), proc: filepath.Join(t.TempDir(), "proc")}
	require.NoError(t, os.MkdirAll(filepath.Join(n.proc, "net"), 0o755))
	n.write(t, n.proc, "net/tcp", tcpHeader)
	n.write(t, n.proc, "net/tcp6", tcpHeader)
	return n
}

func (n node) write(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o755))
}

// process adds a process that holds the terminals given as fd to path, and a thread blocked in
// syscall number nr on fd readFD (-1 for a thread that is running).
func (n node) process(t *testing.T, pid, comm string, fds map[string]string, nr, readFD int) {
	t.Helper()
	dir := filepath.Join(n.proc, pid)
	n.write(t, dir, "comm", comm+"\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
	for fd, target := range fds {
		require.NoError(t, os.Symlink(target, filepath.Join(dir, "fd", fd)))
	}
	state := "running\n"
	if readFD >= 0 {
		state = fmtSyscall(nr, readFD)
	}
	n.write(t, dir, "task/"+pid+"/syscall", state)
}

func fmtSyscall(nr, fd int) string {
	return fmt.Sprintf("%d 0x%x 0x7ffd 0x1000 0x0 0x0 0x0 0x7ffd 0x4000\n", nr, fd)
}

func readNumber() int {
	if runtime.GOARCH == "arm64" {
		return 63
	}
	return 0
}

func (n node) probe(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer

	err := loginprobe.Run(loginprobe.Options{Root: n.root, Proc: n.proc, Skip: []string{filepath.Join(n.root, "var/lib/naru")}}, slog.New(slog.NewTextHandler(&out, nil)))

	require.NoError(t, err)
	return out.String()
}

func TestRun_ReportsEachListeningSocketAndOnlyThose(t *testing.T) {
	n := newNode(t)
	n.write(t, n.proc, "net/tcp", tcpHeader+
		"   0: 00000000:C350 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1\n"+
		"   1: 0100007F:1388 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2\n"+
		"   2: 0100007F:1388 0100007F:9999 01 00000000:00000000 00:00000000 00000000     0        0 3\n")
	n.write(t, n.proc, "net/tcp6", tcpHeader+
		"   0: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4\n")

	out := n.probe(t)

	assert.Contains(t, out, "kind=listener addr=0.0.0.0 port=50000")
	assert.Contains(t, out, "kind=listener addr=127.0.0.1 port=5000")
	assert.Contains(t, out, "kind=listener addr=::1 port=8080")
	assert.Contains(t, out, "kind=done listeners=3", "an established connection is not a listener")
}

func TestRun_ReportsAShellALoginAndALinkNamedLikeOneButNotWhatIsUnderASkippedDirectory(t *testing.T) {
	n := newNode(t)
	n.write(t, n.root, "bin/mvmd", "x")
	n.write(t, n.root, "bin/sh", "x")
	n.write(t, n.root, "usr/sbin/sshd", "x")
	require.NoError(t, os.Symlink("naru-init", filepath.Join(n.root, "bin", "getty")))
	n.write(t, n.root, "var/lib/naru/inputs/bin/bash", "staged by a test")

	out := n.probe(t)

	assert.Contains(t, out, "kind=program path="+filepath.Join(n.root, "bin/sh"))
	assert.Contains(t, out, "kind=program path="+filepath.Join(n.root, "usr/sbin/sshd"))
	assert.Contains(t, out, "kind=program path="+filepath.Join(n.root, "bin/getty"), "a link is a way in")
	assert.NotContains(t, out, "bin/mvmd")
	assert.NotContains(t, out, "bash")
	assert.Contains(t, out, "programs=3")
}

func TestRun_ReportsNoProgramForAnImageWithNone(t *testing.T) {
	n := newNode(t)
	n.write(t, n.root, "bin/naru-init", "x")

	assert.Contains(t, n.probe(t), "kind=done listeners=0 programs=0 tty-holders=0 readers=0")
}

func TestRun_ReportsAProcessThatHoldsATerminalButNotPID1(t *testing.T) {
	n := newNode(t)
	n.process(t, "1", "naru-init", map[string]string{"0": "/dev/console", "1": "/dev/console"}, 0, -1)
	n.process(t, "40", "svc", map[string]string{"0": "/dev/null", "3": "/dev/pts/0"}, 0, -1)

	out := n.probe(t)

	assert.Contains(t, out, "kind=tty-holder pid=40 comm=svc path=/dev/pts/0")
	assert.Contains(t, out, "tty-holders=1 readers=0", "PID 1 holds the console the kernel opened and does not read it")
}

func TestRun_ReportsAThreadBlockedReadingATerminalAndNotOneReadingAPipe(t *testing.T) {
	n := newNode(t)
	n.process(t, "1", "naru-init", map[string]string{"0": "/dev/console", "5": "pipe:[7]"}, readNumber(), 0)
	n.process(t, "9", "other", map[string]string{"5": "pipe:[8]"}, readNumber(), 5)

	out := n.probe(t)

	assert.Contains(t, out, "kind=reader pid=1 tid=1 comm=naru-init path=/dev/console")
	assert.Contains(t, out, "readers=1", "a read of a pipe is not a read of the console")
}

func TestRun_FailsWhenTheProcessFileSystemHasNoNetworkTable(t *testing.T) {
	n := node{root: t.TempDir(), proc: t.TempDir()}

	err := loginprobe.Run(loginprobe.Options{Root: n.root, Proc: n.proc}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	require.Error(t, err)
}
