package main

import (
	"context"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/orch/internal/leader"
	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchd"
)

// housekeepEvery is the default pause between the leader's housekeeping passes.
const housekeepEvery = time.Minute

// housekeep runs the leader's etcd housekeeping (ADR 0088) every `every` until ctx ends. A
// member that does not lead does nothing, so one member at a time is defragmented or removed.
func housekeep(ctx context.Context, log *slog.Logger, o *orchd.Orchd, quota int64, departAfter, every time.Duration) {
	var kv clientv3.KV
	h := &member.Housekeeper{Quota: quota, Departure: departAfter, Now: time.Now, Log: log}
	// The fence is a write guarded by the election key, which fails once another leader holds it.
	h.Fence = func(ctx context.Context) error {
		_, err := leader.Guard(kv).Txn(ctx).Commit()
		return err
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cl := o.Etcd()
		led, ok := o.Term()
		if cl == nil || !ok {
			h.Forget()
			continue
		}
		h.Client, kv = cl, cl
		if err := h.Pass(led); err != nil && ctx.Err() == nil {
			log.Error("etcd housekeeping failed", "err", err.Error())
		}
	}
}
