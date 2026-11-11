package standin

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForSocket_ReturnsOnceTheSocketAcceptsAConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
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

	err := waitForSocket(context.Background(), path, 5*time.Second)

	assert.NoError(t, err)
}

func TestWaitForSocket_FailsWhenNothingListensBeforeTheTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sock")

	err := waitForSocket(context.Background(), path, 300*time.Millisecond)

	require.Error(t, err)
	assert.ErrorContains(t, err, "did not accept a connection")
}
