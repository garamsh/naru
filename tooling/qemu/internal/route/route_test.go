package route_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/route"
)

// echo serves a TCP echo on loopback and returns its address.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func serve(t *testing.T, allow route.Allow) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = route.Serve(ln, allow) }()
	return ln.Addr().String()
}

func TestForwardCarriesAConnectionToAnAllowedTargetAndBack(t *testing.T) {
	target := echo(t)
	via := serve(t, route.WithinPrefix(netip.MustParsePrefix("127.0.0.0/8")))
	addr, stop, err := route.Forward(context.Background(), via, target)
	require.NoError(t, err)
	defer stop()

	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = io.WriteString(c, "hello")
	require.NoError(t, err)
	require.NoError(t, c.(*net.TCPConn).CloseWrite())
	got, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

func TestDialIsRefusedForATargetOutsideTheAllowedPrefix(t *testing.T) {
	target := echo(t)
	via := serve(t, route.WithinPrefix(netip.MustParsePrefix("10.77.0.0/16")))
	_, err := route.Dial(via, target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused")
	assert.Contains(t, err.Error(), "is not in 10.77.0.0/16")
}

func TestDialIsRefusedForAMalformedTarget(t *testing.T) {
	via := serve(t, route.WithinPrefix(netip.MustParsePrefix("0.0.0.0/0")))
	_, err := route.Dial(via, "not-an-address")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused")
}
