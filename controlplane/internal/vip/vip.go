// Package vip holds the control plane's virtual address: of the control-plane Nodes whose API server
// answers, the first in the order the configuration lists them holds it (ADR 0024).
package vip

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// Host is what the holder does to its own Node.
type Host interface {
	// Holds reports whether the Node has the address.
	Holds() (bool, error)
	// Acquire adds the address to the Node and announces it on the network.
	Acquire() error
	// Announce tells the network again that the Node has the address.
	Announce() error
	// Release removes the address from the Node.
	Release() error
}

// Prober reports whether the API server of the control-plane Node at addr answers.
type Prober func(ctx context.Context, addr netip.Addr) bool

// Config says which Nodes there are and how often they are looked at.
type Config struct {
	// Members are the control-plane Nodes' addresses in priority order, Self's among them.
	Members []netip.Addr
	Self    netip.Addr
	// Interval is the time between rounds. A member is alive after one answer and dead after Misses
	// rounds with none; one that never answered is dead. A holder announces again every
	// AnnounceEvery rounds.
	Interval      time.Duration
	Misses        int
	AnnounceEvery int
	// Settle is the number of rounds after the start in which the address is not taken, so that a
	// member that starts with the others does not take it for the seconds before the first member's
	// API server answers.
	Settle int
}

// Run looks at the members every Interval until ctx ends, and keeps the address on this Node exactly
// while this Node is the first live member. On a Node that stops answering, or when a member before
// this one answers, the address is released; it is not released when Run ends, so that a Node
// restarting the service keeps the address in between.
func Run(ctx context.Context, cfg Config, host Host, probe Prober, log *slog.Logger) {
	misses := make(map[netip.Addr]int, len(cfg.Members)) // absent: has never answered
	rounds := 0
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		answered := Probe(ctx, cfg.Members, probe)
		for _, m := range cfg.Members {
			switch _, seen := misses[m]; {
			case answered[m]:
				misses[m] = 0
			case seen:
				misses[m]++
			}
		}
		rounds++
		step(cfg, host, misses, rounds, log)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Holder is the first member that is alive: it answered, and has not missed misses rounds since.
// ok is false when none is.
func Holder(members []netip.Addr, missed map[netip.Addr]int, misses int) (holder netip.Addr, ok bool) {
	for _, m := range members {
		if n, seen := missed[m]; seen && n < misses {
			return m, true
		}
	}
	return netip.Addr{}, false
}

func step(cfg Config, host Host, missed map[netip.Addr]int, rounds int, log *slog.Logger) {
	holder, ok := Holder(cfg.Members, missed, cfg.Misses)
	holds, err := host.Holds()
	if err != nil {
		log.Error("naru-vip: read the Node's addresses", "err", err)
		return
	}
	switch {
	case ok && holder == cfg.Self && !holds && rounds <= cfg.Settle:
	case ok && holder == cfg.Self && !holds:
		if err := host.Acquire(); err != nil {
			log.Error("naru-vip: acquire the address", "err", err)
			return
		}
		log.Info("naru-vip: holding the address", "holder", holder)
	case ok && holder == cfg.Self && rounds%cfg.AnnounceEvery == 0:
		if err := host.Announce(); err != nil {
			log.Error("naru-vip: announce the address", "err", err)
		}
	case holds && !(ok && holder == cfg.Self):
		if err := host.Release(); err != nil {
			log.Error("naru-vip: release the address", "err", err)
			return
		}
		log.Info("naru-vip: released the address", "holder", holder, "live", ok)
	}
}

// Probe asks every member at once and returns who answered.
func Probe(ctx context.Context, members []netip.Addr, probe Prober) map[netip.Addr]bool {
	type result struct {
		addr netip.Addr
		up   bool
	}
	ch := make(chan result, len(members))
	for _, m := range members {
		go func() { ch <- result{m, probe(ctx, m)} }()
	}
	up := make(map[netip.Addr]bool, len(members))
	for range members {
		r := <-ch
		up[r.addr] = r.up
	}
	return up
}
