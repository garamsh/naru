// Package unixfwd listens on a unix socket of the test host and forwards each connection to a TCP
// address: naructl's signer commands take the path of the signer's socket, and the signer's socket is
// in a guest, which naru-sockproxy exposes on a TCP port the harness forwards (docs/architecture/harness.md).
package unixfwd

import (
	"context"
	"io"
	"net"
	"os"
)

// Listen serves path until the returned stop is called. A connection to path is a connection to addr.
func Listen(path, addr string) (stop func(), err error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go forward(c, addr)
		}
	}()
	return func() { _ = ln.Close() }, nil
}

func forward(c net.Conn, addr string) {
	defer func() { _ = c.Close() }()
	var d net.Dialer
	t, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return
	}
	defer func() { _ = t.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(t, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, t); done <- struct{}{} }()
	<-done
}
