package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/pki"
	"github.com/garamsh/naru/lib/init/pkitest"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
)

const nodeNetwork = `version: 1
config:
  - type: physical
    name: eth0
    mac_address: '02:00:00:00:00:01'
    subnets:
      - type: static
        address: '10.78.0.1'
        netmask: '255.255.255.0'
`

var epoch = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// clock hands every After to the test, which decides when it fires.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan chan time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers <- ch
	return ch
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// node is the Host and the Services of a Node whose second service cannot start.
type node struct {
	userData string
	restarts chan string
}

func (n *node) ReadSeed() ([]byte, []byte, error) {
	return []byte(n.userData), []byte(nodeNetwork), nil
}
func (n *node) ApplyNetwork(netcfg.Config) error    { return nil }
func (n *node) MountDataDisk(config.DataDisk) error { return nil }
func (n *node) FormatBlankDataDisk(config.DataDisk, *slog.Logger) error {
	return nil
}
func (n *node) EnableForwarding([]config.Family) error {
	return nil
}
func (n *node) AddRoute(config.Route) error                       { return nil }
func (n *node) DeleteRoute(config.Route) error                    { return nil }
func (n *node) WriteFiles([]config.File) error                    { return nil }
func (n *node) WriteRegistries(config.Spec) error                 { return nil }
func (n *node) SetAsidePair(config.KeptPair) (bool, error)        { return false, nil }
func (n *node) PrepareStorage(config.Storage, *slog.Logger) error { return nil }
func (n *node) MountExtensions([]config.Extension) error          { return nil }
func (n *node) SealRoot() error                                   { return nil }
func (n *node) SyncTime([]string) (timesync.Result, error) {
	return timesync.Result{}, nil
}
func (n *node) RunTime(ctx context.Context, _ *slog.Logger) { <-ctx.Done() }
func (n *node) TimeStatus() timesync.Status                 { return timesync.Status{} }
func (n *node) Prepare() error                              { return nil }
func (n *node) Restart(name string)                         { n.restarts <- name }
func (n *node) Start(svc config.Service) error {
	if svc.Name == "b" {
		return errors.New("b cannot start")
	}
	return nil
}

func TestBootAndRenew_RenewsTheCertificatesOfAServiceThatStartedBeforeABootRefusal(t *testing.T) {
	ca := pkitest.NewCA(t, epoch.Add(-24*time.Hour), epoch.Add(365*24*time.Hour))
	n := &node{restarts: make(chan string, 4), userData: `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: a
      binary: /usr/bin/a
    - name: b
      binary: /usr/bin/b
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/mvmd
  pki:
    authorities:
      - name: ca
        certificate: |
` + pkitest.Indent(ca.Cert, 10) + "        key: |\n" + pkitest.Indent(ca.Key, 10) + `        certificatePath: /run/pki/ca.pem
    certificates:
      - name: a
        authority: ca
        certificatePath: /run/pki/a.pem
        keyPath: /run/pki/a.key
        commonName: a
        usage: client
        validity: 3h
        restart: [a]
`}
	clk := &clock{now: epoch, timers: make(chan chan time.Time, 4)}
	issuer := pki.New(t.TempDir(), clk, n, slog.New(slog.NewTextHandler(io.Discard, nil)))

	stop, booted := bootAndRenew(n, issuer, n, store.New(t.TempDir()), noAPI{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)
	assert.Nil(t, booted, "the boot was refused, so there is no effective configuration")

	var timer chan time.Time
	select {
	case timer = <-clk.timers:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot was refused after b failed to start, and nothing renews a's certificate")
	}
	clk.advance(2 * time.Hour)
	timer <- time.Time{}
	select {
	case name := <-n.restarts:
		assert.Equal(t, "a", name)
	case <-time.After(5 * time.Second):
		require.Fail(t, "the renewal restarted nothing")
	}
}

// noAPI is a Node API that is never served.
type noAPI struct{}

func (noAPI) Start(store.Stored) error { return nil }

func (noAPI) UseTime(func() timesync.Status) {}
