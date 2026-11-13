// Command naru-netecho is the service naru's QEMU tests run to see what crosses the network between
// two Nodes: it sends datagrams from a Machine's address, reports the source address datagrams
// arrive with, and reports the routes the kernel has through a gateway.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/garamsh/naru/tooling/qemu/internal/netecho"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

func main() {
	var opts netecho.Options
	var listen, send list
	flag.Var(&listen, "listen", "UDP address <address>:<port> to receive on, repeatable")
	flag.Var(&send, "send", "datagram to send, <from>=<address>:<port>, repeatable")
	flag.BoolVar(&opts.Alias, "alias", false, "give the loopback every listening address and every source")
	flag.BoolVar(&opts.Routes, "routes", false, "report the routes through a gateway as they appear and go")
	flag.StringVar(&opts.After, "after", "", "Unix socket that must accept a connection before datagrams are sent")
	flag.Parse()
	opts.Listen, opts.Send = listen, send
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := netecho.Run(ctx, opts, slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-netecho")); err != nil {
		slog.Error("netecho failed", "err", err)
		os.Exit(1)
	}
}
