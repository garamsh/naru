package hostclient_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/netrec"
)

// A Create that outlasts the http.Client's own Timeout still succeeds, once: the deadline of a call is its context's.
func TestCreateOutlastingTheBaseClientsTimeoutSucceedsOnce(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
			return
		}
		_, _ = w.Write([]byte(`{"microvms":[]}`))
	}))
	t.Cleanup(srv.Close)
	inv := inventory{{ID: "h", Address: srv.Listener.Addr().String()}}
	base := &http.Client{Timeout: 50 * time.Millisecond, Transport: &http.Transport{TLSClientConfig: insecure()}}
	c := hostclient.New(inv, base, nil)
	_, err := c.List(t.Context())
	require.NoError(t, err)

	err = c.Create(t.Context(), "h", groups.MachineSpec{ID: "a-1", Template: groups.Template{Form: groups.FormImage, CPUs: 1, MemoryMiB: 128, Version: "r@sha256:abc", RootDiskBytes: 1 << 30}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, posts.Load())
}

type countingInventory struct {
	hosts []orchapi.Host
	reads atomic.Int32
}

func (i *countingInventory) Hosts(context.Context) ([]orchapi.Host, error) {
	i.reads.Add(1)
	return i.hosts, nil
}

// One pass reads the inventory once, however many hosts it calls.
func TestAPassReadsTheInventoryOnceForNHosts(t *testing.T) {
	const n = 8
	inv := &countingInventory{}
	for i := range n {
		inv.hosts = append(inv.hosts, orchapi.Host{ID: fmt.Sprintf("h%d", i), Address: newHost(t).addr()})
	}
	c := hostclient.New(inv, newTLSBase(), nil)

	_, err := c.List(t.Context())
	require.NoError(t, err)
	for _, h := range inv.hosts {
		require.NoError(t, c.Delete(t.Context(), h.ID, "a"))
		require.NoError(t, c.Stop(t.Context(), h.ID, "a"))
		require.NoError(t, c.Start(t.Context(), h.ID, "a"))
		_, _, err := c.InspectFence(t.Context(), h.ID, "db-1")
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, inv.reads.Load())
}

func newTLSBase() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}
}

// flaky answers Tunnel and MicroVMs from ok, or fails when it is false.
type flaky struct {
	mu sync.Mutex
	ok bool
}

func (f *flaky) set(ok bool) { f.mu.Lock(); f.ok = ok; f.mu.Unlock() }

func (f *flaky) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ok {
		return errors.New("no answer")
	}
	return nil
}

func (f *flaky) Tunnel(context.Context) (netrec.Tunnel, error) {
	return netrec.Tunnel{PublicKey: "k"}, f.check()
}

func (f *flaky) MicroVMs(context.Context) ([]netrec.MicroVM, error) {
	return []netrec.MicroVM{{Name: "vm"}}, f.check()
}
func (f *flaky) PutNetwork(context.Context, string, netrec.NetworkState) error { return nil }
func (f *flaky) PutPeers(context.Context, string, string, []netrec.Peer) error { return nil }
func (f *flaky) Networks(context.Context) ([]string, error)                    { return nil, nil }
func (f *flaky) DeleteNetwork(context.Context, string) error                   { return nil }

// A host's probes must fail three times in a row before the reconciler hears of it; fewer answer with the last answer.
func TestNetHostsKeepThePeersOfAHostUntilThreeProbesInARowFail(t *testing.T) {
	api := &flaky{ok: true}
	inv := &invFake{hosts: []orchapi.Host{{ID: "h1", Address: "192.0.2.1:7443"}}}
	h := hostclient.NewNetHosts(inv, func(_, _ string) hostclient.NetAPI { return api })
	_, err := h.List(t.Context())
	require.NoError(t, err)

	_, err = h.Tunnel(t.Context(), "h1")
	require.NoError(t, err)
	_, err = h.MicroVMs(t.Context(), "h1")
	require.NoError(t, err)

	api.set(false)
	for i := range 2 {
		tun, err := h.Tunnel(t.Context(), "h1")
		require.NoError(t, err, "failure %d keeps the last answer", i+1)
		assert.Equal(t, "k", tun.PublicKey)
		vms, err := h.MicroVMs(t.Context(), "h1")
		require.NoError(t, err)
		assert.Equal(t, "vm", vms[0].Name)
	}
	_, err = h.Tunnel(t.Context(), "h1")
	require.Error(t, err, "the third failure in a row is reported")

	api.set(true)
	_, err = h.Tunnel(t.Context(), "h1")
	require.NoError(t, err)
	api.set(false)
	_, err = h.Tunnel(t.Context(), "h1")
	require.NoError(t, err, "a success resets the count")
}

// A host that never answered has no last answer to keep.
func TestNetHostsReportAHostThatNeverAnswered(t *testing.T) {
	api := &flaky{}
	inv := &invFake{hosts: []orchapi.Host{{ID: "h1", Address: "192.0.2.1:7443"}}}
	h := hostclient.NewNetHosts(inv, func(_, _ string) hostclient.NetAPI { return api })
	_, err := h.List(t.Context())
	require.NoError(t, err)
	_, err = h.Tunnel(t.Context(), "h1")
	require.Error(t, err)
}
