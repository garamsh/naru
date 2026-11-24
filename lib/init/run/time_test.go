package run

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
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

func TestBootAndKeep_KeepsTheClockForAsLongAsTheNodeRunsAndGivesTheAPIItsStatus(t *testing.T) {
	n := &timedNode{
		node:   &node{userData: "apiVersion: naru.garam.sh/v1alpha1\nkind: NodeConfig\nmetadata:\n  name: node-1\nspec:\n  dataDisk:\n    label: NARUDATA\n    mountPoint: /var/lib/mvmd\n  time:\n    servers: [ntp.example]\n"},
		ran:    make(chan struct{}),
		ended:  make(chan struct{}),
		status: timesync.Status{Servers: []string{"ntp.example:123"}},
	}
	api := &statusAPI{}

	stop, booted, _ := bootAndKeep(n, n, store.New(t.TempDir()), api, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

func TestBootAndKeep_KeepsTheClockAfterABootRefusal(t *testing.T) {
	n := &timedNode{
		node:  &node{userData: "apiVersion: naru.garam.sh/v1alpha1\nkind: NodeConfig\nmetadata:\n  name: node-1\nspec:\n  services:\n    - name: a\n      binary: /usr/bin/a\n    - name: b\n      binary: /usr/bin/b\n  dataDisk:\n    label: NARUDATA\n    mountPoint: /var/lib/mvmd\n  time:\n    servers: [ntp.example]\n"},
		ran:   make(chan struct{}),
		ended: make(chan struct{}),
	}

	stop, booted, refusal := bootAndKeep(n, n, store.New(t.TempDir()), &statusAPI{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)

	require.Error(t, refusal, "b cannot start, so the boot is refused")
	assert.Nil(t, booted, "the boot was refused, so there is no effective configuration")
	select {
	case <-n.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot was refused and the clock does not run")
	}
}
