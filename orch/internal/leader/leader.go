// Package leader elects the one member of orch that runs the reconcilers and writes derived state.
package leader

import (
	"context"
	"errors"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// Election is the key of the election in etcd.
const Election = "/naru/orch/leader"

// retryAfter is how long Run waits after a failed session or campaign.
const retryAfter = time.Second

// Run campaigns as name until ctx ends. While this member leads it calls lead with a context that
// ends when the lease is lost; when lead returns, or the lease is lost, it campaigns again. A
// stopped leader's lease lapses after ttl, which bounds how long another member takes over.
func Run(ctx context.Context, cl *clientv3.Client, name string, ttl time.Duration, log *slog.Logger, lead func(ctx context.Context)) {
	for ctx.Err() == nil {
		if err := term(ctx, cl, name, ttl, lead); err != nil && ctx.Err() == nil {
			log.Warn("leader election failed", "member", name, "err", err.Error())
			select {
			case <-ctx.Done():
			case <-time.After(retryAfter):
			}
		}
	}
}

func term(ctx context.Context, cl *clientv3.Client, name string, ttl time.Duration, lead func(context.Context)) error {
	sess, err := concurrency.NewSession(cl, concurrency.WithTTL(int(ttl.Seconds())), concurrency.WithContext(ctx))
	if err != nil {
		return err
	}
	defer sess.Orphan()
	if err := concurrency.NewElection(sess, Election).Campaign(ctx, name); err != nil {
		return err
	}
	led, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-led.Done():
		}
	}()
	lead(led)
	return nil
}

// Current returns the name of the member that leads, or "" when none does.
func Current(ctx context.Context, cl *clientv3.Client) (string, error) {
	sess, err := concurrency.NewSession(cl, concurrency.WithTTL(5), concurrency.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()
	resp, err := concurrency.NewElection(sess, Election).Leader(ctx)
	if errors.Is(err, concurrency.ErrElectionNoLeader) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(resp.Kvs[0].Value), nil
}
