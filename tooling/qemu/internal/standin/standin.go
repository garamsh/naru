// Package standin is the service naru's QEMU tests configure in place of
// gaon's Node Agent. It reports, on the console, the two facts the tests read
// of a Node: that the data disk is mounted and that mvmd's socket accepts
// connections. It then runs until it is stopped.
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
	// Socket is the Unix socket the stand-in waits to accept a connection.
	Socket string
	// Mount is a directory the stand-in reports as mounted, if not empty.
	Mount string
	// Timeout bounds the wait for the socket.
	Timeout time.Duration
}

// Run reports the mount and the socket, then blocks until ctx ends. It
// returns an error when the directory is not a mount point or the socket does
// not accept a connection within the timeout, so that init restarts it.
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
	if err := waitForSocket(ctx, opts.Socket, opts.Timeout); err != nil {
		return err
	}
	log.Info("socket accepts connections", "path", opts.Socket)
	<-ctx.Done()
	return nil
}

// waitForSocket dials path until a connection is accepted or the timeout passes.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("socket %s did not accept a connection within %s", path, timeout), err)
		case <-time.After(dialInterval):
		}
	}
}
