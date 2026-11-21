package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchd"
)

// housekeepEvery is the pause between the leader's housekeeping passes.
const housekeepEvery = time.Minute

// housekeep runs the leader's etcd housekeeping (ADR 0088) every housekeepEvery until ctx ends. A
// member that does not lead does nothing, so one member at a time is defragmented or removed.
func housekeep(ctx context.Context, log *slog.Logger, o *orchd.Orchd, dataDir string) {
	disk, err := member.DiskBytes(dataDir)
	if err != nil {
		log.Error("etcd housekeeping is off: the data disk cannot be measured", "err", err.Error())
		return
	}
	h := &member.Housekeeper{Quota: member.Quota(disk), Now: time.Now, Log: log}
	t := time.NewTicker(housekeepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cl := o.Etcd()
		if cl == nil || !o.IsLeader() {
			h.Forget()
			continue
		}
		h.Client = cl
		if err := h.Pass(ctx); err != nil && ctx.Err() == nil {
			log.Error("etcd housekeeping failed", "err", err.Error())
		}
	}
}
