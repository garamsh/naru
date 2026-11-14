// Package route is the harness's route from the test host to the address of a microVM on a QEMU Node.
//
// A microVM's address is on its host's Machine prefix, behind the Node's bridge, and QEMU's user-mode
// forward cannot target an address the Node does not own. Serve therefore runs on the Node, on the
// address of its second NIC, and Forward runs on the test host: a connection to a local listener is
// sent through the Node's forward to Serve, which dials the microVM from inside the Node's network.
// The first line of the stream, "host:port", names the target; Serve answers "ok" or "refused: why"
// and then copies both ways.
package route

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DialTimeout is how long either side waits to reach the other.
const DialTimeout = 10 * time.Second

// Allow decides which targets Serve dials: it returns an error for one it refuses.
type Allow func(netip.AddrPort) error

// WithinPrefix allows the targets whose address is in prefix, and no other.
func WithinPrefix(prefix netip.Prefix) Allow {
	return func(t netip.AddrPort) error {
		if !prefix.Contains(t.Addr()) {
			return fmt.Errorf("%s is not in %s", t.Addr(), prefix)
		}
		return nil
	}
}

// Serve accepts connections on ln until ln closes, and serves each as the route's Node side.
func Serve(ln net.Listener, allow Allow) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handle(c, allow)
	}
}

func handle(c net.Conn, allow Allow) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(DialTimeout))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || len(line) > 256 {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	target, err := netip.ParseAddrPort(strings.TrimSpace(line))
	if err == nil {
		err = allow(target)
	}
	if err != nil {
		_, _ = fmt.Fprintf(c, "refused: %v\n", err)
		return
	}
	out, err := net.DialTimeout("tcp", target.String(), DialTimeout)
	if err != nil {
		_, _ = fmt.Fprintf(c, "refused: %v\n", err)
		return
	}
	defer func() { _ = out.Close() }()
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		return
	}
	// br holds what followed the first line, if anything arrived with it.
	splice(c, out, br)
}

// splice copies in to out and out to c until both directions end; in is c, or a reader that
// holds the start of c's stream.
func splice(c, out net.Conn, in io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(out, in)
		closeWrite(out)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, out)
		closeWrite(c)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// Forward listens on 127.0.0.1 on a port of the system's choosing and sends each connection to
// target through the Serve at via, which the Node's forward makes "127.0.0.1:port". It returns the
// listener's address and a stop func. A refusal by Serve closes the connection.
func Forward(ctx context.Context, via, target string) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				out, err := Dial(via, target)
				if err != nil {
					return
				}
				defer func() { _ = out.Close() }()
				splice(c, out, c)
			}()
		}
	}()
	return ln.Addr().String(), cancel, nil
}

// Dial opens a connection to target through the Serve at via and returns it once Serve accepted.
func Dial(via, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", via, DialTimeout)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, target+"\n"); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * DialTimeout))
	// One byte at a time: nothing of the stream behind the answer may be consumed.
	var answer []byte
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(c, one); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("the route to %s: %w", target, err)
		}
		if one[0] == '\n' {
			break
		}
		answer = append(answer, one[0])
		if len(answer) > 256 {
			_ = c.Close()
			return nil, errors.New("the route's answer is too long")
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	if string(answer) != "ok" {
		_ = c.Close()
		return nil, fmt.Errorf("the route to %s: %s", target, answer)
	}
	return c, nil
}
