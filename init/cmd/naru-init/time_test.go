package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/store"
	"github.com/garamsh/naru/init/internal/timesync"
)

// timedNode is a node whose clock runs until its context ends.
type timedNode struct {
	*node
	ran, ended chan struct{}
	status     timesync.Status
}

func (n *timedNode) RunTime(ctx context.Context, _ *slog.Logger) {
	close(n.ran)
	<-ctx.Done()
	close(n.ended)
}

func (n *timedNode) TimeStatus() timesync.Status { return n.status }

// statusAPI records the clock the API was given.
type statusAPI struct {
	noAPI
	status func() timesync.Status
}

func (a *statusAPI) UseTime(status func() timesync.Status) { a.status = status }

func TestBootAndRenew_KeepsTheClockForAsLongAsTheNodeRunsAndGivesTheAPIItsStatus(t *testing.T) {
	n := &timedNode{
		node:   &node{restarts: make(chan string, 4), userData: "apiVersion: naru.garam.sh/v1alpha1\nkind: NodeConfig\nmetadata:\n  name: node-1\nspec:\n  dataDisk:\n    label: NARUDATA\n    mountPoint: /var/lib/mvmd\n  time:\n    servers: [ntp.example]\n"},
		ran:    make(chan struct{}),
		ended:  make(chan struct{}),
		status: timesync.Status{Servers: []string{"ntp.example:123"}},
	}
	clk := &clock{now: epoch, timers: make(chan chan time.Time, 4)}
	issuer := pki.New(t.TempDir(), clk, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := &statusAPI{}

	stop, booted := bootAndRenew(n, issuer, n, store.New(t.TempDir()), api, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NotNil(t, booted)
	select {
	case <-n.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the clock does not run")
	}
	require.NotNil(t, api.status, "the API was not given the clock's status")
	assert.Equal(t, n.status, api.status())
	select {
	case <-n.ended:
		t.Fatal("the clock stopped before the Node did")
	default:
	}

	stop()

	select {
	case <-n.ended:
	default:
		t.Fatal("stop returned before the clock ended")
	}
}
