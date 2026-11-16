package leader_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/leader"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// campaigner runs leader.Run as name on its own client and hands over the context of each term.
type campaigner struct {
	terms chan context.Context
}

func campaign(t *testing.T, cl *clientv3.Client, name string) *campaigner {
	t.Helper()
	c := &campaigner{terms: make(chan context.Context, 8)}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		leader.Run(ctx, cl, name, 3*time.Second, slog.New(slog.DiscardHandler), func(led context.Context) {
			c.terms <- led
			<-led.Done()
		})
	}()
	t.Cleanup(func() { stop(); <-done })
	return c
}

// term returns the context of the latest term the member led, waiting for one that still holds
// when live is true.
func (c *campaigner) term(t *testing.T, live bool) context.Context {
	t.Helper()
	deadline := time.After(30 * time.Second)
	var last context.Context
	for {
		select {
		case ctx := <-c.terms:
			last = ctx
			if !live || ctx.Err() == nil {
				if !live {
					for len(c.terms) > 0 {
						last = <-c.terms
					}
				}
				return last
			}
		case <-deadline:
			t.Fatal("the member never led")
			return nil
		}
	}
}

// leadTo revokes the lease of every leader that is not name until name leads.
func leadTo(t *testing.T, cl *clientv3.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := cl.Get(t.Context(), leader.Election, append([]clientv3.OpOption{clientv3.WithPrefix()}, clientv3.WithFirstCreate()...)...)
		require.NoError(t, err)
		if len(resp.Kvs) == 1 {
			kv := resp.Kvs[0]
			if string(kv.Value) == name {
				return
			}
			_, _ = cl.Revoke(t.Context(), clientv3.LeaseID(kv.Lease))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never led", name)
}

type fixture struct {
	c        *orchtest.Cluster
	etcd     *clientv3.Client
	guard    clientv3.KV
	old, now context.Context
	operator *orchapi.Client
}

// twoTerms leaves a first leader that lost its lease and a second that holds the term.
func twoTerms(t *testing.T) fixture {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	cl := c.Members[0].Etcd()
	a, b := campaign(t, cl, "a"), campaign(t, cl, "b")
	leadTo(t, cl, "a")
	old := a.term(t, false)
	leadTo(t, cl, "b")
	now := b.term(t, true)
	id := c.CA.Issue(t, "alice", identity.RoleOperator)
	return fixture{c: c, etcd: cl, guard: leader.Guard(cl), old: context.WithoutCancel(old), now: now, operator: c.Client(t, 0, &id)}
}

func token(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	term, ok := leader.TermFrom(ctx)
	require.True(t, ok)
	return term.Token
}

func TestGuard_ALeaderThatLostItsLeaseCannotWrite(t *testing.T) {
	f := twoTerms(t)
	assert.Greater(t, token(t, f.now), token(t, f.old), "tokens do not grow from one term to the next")

	// The term that holds the election key writes.
	_, err := f.guard.Put(f.now, "/test/held", "v")
	require.NoError(t, err)

	// The first leader's term is stale: nothing it writes lands, whichever way it writes.
	_, err = f.guard.Put(f.old, "/test/stale", "v")
	assert.ErrorIs(t, err, leader.ErrTermLost)
	_, err = f.guard.Delete(f.old, "/test/held")
	assert.ErrorIs(t, err, leader.ErrTermLost)
	_, err = f.guard.Do(f.old, clientv3.OpPut("/test/stale-do", "v"))
	assert.ErrorIs(t, err, leader.ErrTermLost)
	_, err = f.guard.Txn(f.old).Then(clientv3.OpPut("/test/stale-txn", "v")).Commit()
	assert.ErrorIs(t, err, leader.ErrTermLost)

	got, err := f.etcd.Get(t.Context(), "/test/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1, "a stale write landed")
	assert.Equal(t, "/test/held", string(got.Kvs[0].Key))
}

func TestGuard_ACompareOfTheCallerThatFailsIsNotALostTerm(t *testing.T) {
	f := twoTerms(t)
	_, err := f.guard.Put(f.now, "/test/cas", "1")
	require.NoError(t, err)
	resp, err := f.guard.Txn(f.now).
		If(clientv3.Compare(clientv3.Value("/test/cas"), "=", "other")).
		Then(clientv3.OpPut("/test/cas", "2")).Commit()
	require.NoError(t, err)
	assert.False(t, resp.Succeeded)
	got, err := f.etcd.Get(t.Context(), "/test/cas")
	require.NoError(t, err)
	assert.Equal(t, "1", string(got.Kvs[0].Value))
	assert.NoError(t, f.now.Err(), "the caller's failed compare ended the term")
}

func TestGuard_AWriteWithNoTermFails(t *testing.T) {
	f := twoTerms(t)
	_, err := f.guard.Put(t.Context(), "/test/none", "v")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, leader.ErrTermLost)
}

func TestGuard_TheInventoryStoreBehindItRefusesAStaleLeadersEvent(t *testing.T) {
	f := twoTerms(t)
	stale := invetcd.New(f.guard)
	err := stale.AppendEvent(f.old, orchapi.Event{Time: time.Now(), Type: "t", Subject: "s"})
	assert.ErrorIs(t, err, leader.ErrTermLost)
	require.NoError(t, stale.AppendEvent(f.now, orchapi.Event{Time: time.Now(), Type: "t", Subject: "s"}))
	evs, err := invetcd.New(f.etcd).ListEvents(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, evs, 1)
}

func TestGuard_AnOperatorWriteOnTheClusterSucceedsWhileTheStaleTermIsRefused(t *testing.T) {
	f := twoTerms(t)
	_, err := f.guard.Put(f.old, "/test/stale", "v")
	require.ErrorIs(t, err, leader.ErrTermLost)
	// An operator's request is served by any member through the plain client, with no term.
	_, err = f.operator.CreateEnrollmentKey(t.Context(), f.c.Clock.Now().Add(time.Hour))
	assert.NoError(t, err)
}
