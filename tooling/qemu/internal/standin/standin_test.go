package standin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForDial_ReturnsOnceTheSocketAcceptsAConnection(t *testing.T) {
	path := shortSocketPath(t, "s.sock")
	go func() {
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("unix", path)
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = l.Close() })
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	err := waitForDial(context.Background(), "unix", path, 5*time.Second)

	assert.NoError(t, err)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestRun_ReportsThatAPeerAcceptsConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peerAddr := freeAddr(t)
	peerLog := &lockedBuffer{}
	go func() {
		_ = Run(ctx, Options{Listen: peerAddr, Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(peerLog, nil)))
	}()
	log := &lockedBuffer{}
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, Options{Peer: peerAddr, Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(log, nil)))
	}()

	require.Eventually(t, func() bool { return strings.Contains(log.String(), "peer accepts connections") }, 5*time.Second, 20*time.Millisecond)
	assert.Contains(t, log.String(), "running")
	cancel()
	assert.NoError(t, <-done)
}

func TestRun_FailsWhenThePeerNeverAccepts(t *testing.T) {
	err := Run(context.Background(), Options{Peer: freeAddr(t), Timeout: 300 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.Error(t, err)
	assert.ErrorContains(t, err, "did not accept a connection")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestWaitForDial_FailsWhenNothingListensBeforeTheTimeout(t *testing.T) {
	path := shortSocketPath(t, "absent.sock")

	err := waitForDial(context.Background(), "unix", path, 300*time.Millisecond)

	require.Error(t, err)
	assert.ErrorContains(t, err, "did not accept a connection")
}

// shortSocketPath is a socket path in a directory removed with the test. t.TempDir carries the test's
// name, which with a long TMPDIR passes the 107 bytes a unix socket path may have.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}
