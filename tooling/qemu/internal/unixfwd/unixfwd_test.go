package unixfwd_test

import (
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/unixfwd"
)

func TestListen_ForwardsAUnixConnectionToTheTCPAddress(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = echo.Close() }()
	go func() {
		c, err := echo.Accept()
		if err == nil {
			_, _ = io.Copy(c, c)
		}
	}()
	sock := filepath.Join(t.TempDir(), "s.sock")
	stop, err := unixfwd.Listen(sock, echo.Addr().String())
	require.NoError(t, err)
	defer stop()

	c, err := net.Dial("unix", sock)
	require.NoError(t, err)
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)

	assert.Equal(t, "ping", string(buf))
}
