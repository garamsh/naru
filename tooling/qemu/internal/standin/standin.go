// Package standin is the service naru's QEMU tests configure in place of
// gaon's Node Agent and control plane. It reports, on the console, the facts
// the tests read of a Node: that the data disk is mounted, that mvmd's socket
// accepts connections, and that a peer Node's stand-in accepts connections
// over the network. It then runs until it is stopped.
package standin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"
)

const (
	mountInfo    = "/proc/self/mountinfo"
	dialInterval = 100 * time.Millisecond
)

// Options says what the stand-in reports.
type Options struct {
	// Socket is the Unix socket the stand-in waits to accept a connection, if not empty.
	Socket string
	// Mount is a directory the stand-in reports as mounted, if not empty.
	Mount string
	// Listen is a TCP address the stand-in accepts connections on, if not empty.
	Listen string
	// Peer is a TCP address of another Node's stand-in that the stand-in waits
	// to accept a connection, if not empty.
	Peer string
	// Timeout bounds each wait.
	Timeout time.Duration
}

// Run reports the mount, the listener, the socket and the peer, then blocks
// until ctx ends. It returns an error when the directory is not a mount
// point, or the socket or the peer does not accept a connection within the
// timeout, so that init restarts it.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	log.Info("started")
	if opts.Mount != "" {
		f, err := os.Open(mountInfo)
		if err != nil {
			return err
		}
		m, err := mountOf(f, opts.Mount)
		_ = f.Close()
		if err != nil {
			return err
		}
		log.Info("data disk is mounted", "dir", m.Dir, "fstype", m.FSType, "source", m.Source, "options", m.Options)
	}
	if opts.Listen != "" {
		l, err := net.Listen("tcp", opts.Listen)
		if err != nil {
			return err
		}
		defer func() { _ = l.Close() }()
		go acceptAndClose(l)
		log.Info("listening", "addr", opts.Listen)
	}
	if opts.Socket != "" {
		if err := waitForDial(ctx, "unix", opts.Socket, opts.Timeout); err != nil {
			return err
		}
		log.Info("socket accepts connections", "path", opts.Socket)
	}
	if opts.Peer != "" {
		if err := waitForDial(ctx, "tcp", opts.Peer, opts.Timeout); err != nil {
			return err
		}
		log.Info("peer accepts connections", "addr", opts.Peer)
	}
	log.Info("running")
	<-ctx.Done()
	return nil
}

func acceptAndClose(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// waitForDial dials the address until a connection is accepted or the timeout passes.
func waitForDial(ctx context.Context, network, addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s %s did not accept a connection within %s", network, addr, timeout), err)
		case <-time.After(dialInterval):
		}
	}
}
