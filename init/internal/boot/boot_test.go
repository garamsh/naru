package boot_test

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/boot"
	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/netcfg"
	"github.com/garamsh/naru/init/internal/pkitest"
	"github.com/garamsh/naru/init/internal/store"
	"github.com/garamsh/naru/init/internal/timesync"
)

const validNetwork = `version: 1
config:
  - type: physical
    name: eth0
    mac_address: '02:00:00:00:00:01'
    subnets:
      - type: static
        address: '10.78.0.1'
        netmask: '255.255.255.0'
`

const validConfig = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  controlPlane: false
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/mvmd
  network:
    forwarding: [ipv4]
  files:
    - path: /run/mvmd/config.json
      content: "{}"
  services:
    - name: mvmd
      binary: /usr/bin/mvmd
    - name: gaon
      binary: /usr/bin/gaon
      controlPlane: true
`

// recorder is both Host and Services and records the order of their calls.
type recorder struct {
	raw        string
	network    string
	readErr    error
	networkErr error
	filesErr   error
	diskErr    error
	forwardErr error
	prepareErr error
	sealErr    error
	issueErr   error
	timeErr    error
	routeErr   error
	startErr   error
	apiErr     error
	api        *store.Stored

	stored  *store.Stored
	loadErr error
	saved   []store.Stored
	mounted config.DataDisk
	started []string

	calls     []string
	forwarded []config.Family
	applied   netcfg.Config
	written   []config.File
	issuedPKI config.PKI
	issuedTo  []netip.Addr

	timeServers   []string
	routes        []config.Route
	registries    config.Spec
	setAside      bool
	setAsideErr   error
	registriesErr error
}

func (r *recorder) ReadSeed() ([]byte, []byte, error) {
	r.calls = append(r.calls, "read")
	return []byte(r.raw), []byte(r.network), r.readErr
}

func (r *recorder) ApplyNetwork(c netcfg.Config) error {
	r.calls = append(r.calls, "network")
	r.applied = c
	return r.networkErr
}

func (r *recorder) WriteFiles(f []config.File) error {
	r.calls = append(r.calls, "files")
	r.written = f
	return r.filesErr
}

func (r *recorder) MountDataDisk(d config.DataDisk) error {
	r.calls = append(r.calls, "mount")
	r.mounted = d
	return r.diskErr
}

func (r *recorder) EnableForwarding(f []config.Family) error {
	r.calls = append(r.calls, "forward")
	r.forwarded = f
	return r.forwardErr
}

func (r *recorder) AddRoute(rt config.Route) error {
	r.calls = append(r.calls, "route "+rt.To.String())
	r.routes = append(r.routes, rt)
	return r.routeErr
}

func (r *recorder) WriteRegistries(spec config.Spec) error {
	r.calls = append(r.calls, "registries")
	r.registries = spec
	return r.registriesErr
}

func (r *recorder) SetAsidePair(p config.KeptPair) (bool, error) {
	r.calls = append(r.calls, "pair "+p.File)
	return r.setAside, r.setAsideErr
}

func (r *recorder) SyncTime(servers []string) (timesync.Result, error) {
	r.calls = append(r.calls, "time")
	r.timeServers = servers
	return timesync.Result{Server: "ntp.example:123", Correction: timesync.Step}, r.timeErr
}

func (r *recorder) SealRoot() error {
	r.calls = append(r.calls, "seal")
	return r.sealErr
}

func (r *recorder) Issue(spec config.PKI, addrs []netip.Addr) error {
	r.calls = append(r.calls, "issue")
	r.issuedPKI = spec
	r.issuedTo = addrs
	return r.issueErr
}

func (r *recorder) Prepare() error {
	r.calls = append(r.calls, "prepare")
	return r.prepareErr
}

func (r *recorder) Start(s config.Service) error {
	r.calls = append(r.calls, "start "+s.Name)
	r.started = append(r.started, s.Name)
	return r.startErr
}

// apiProbe is the Node API of a recorder: it records that it started, and with what.
type apiProbe struct{ r *recorder }

func (a apiProbe) Start(st store.Stored) error {
	a.r.calls = append(a.r.calls, "api")
	a.r.api = &st
	return a.r.apiErr
}

func (r *recorder) Load() (store.Stored, error) {
	r.calls = append(r.calls, "load")
	if r.stored == nil {
		return store.Stored{}, store.ErrNotFound
	}
	return *r.stored, r.loadErr
}

func (r *recorder) Save(st store.Stored) error {
	r.calls = append(r.calls, "save")
	r.saved = append(r.saved, st)
	return nil
}

func run(r *recorder) error {
	_, err := runStored(r)
	return err
}

func runStored(r *recorder) (store.Stored, error) {
	if r.network == "" {
		r.network = validNetwork
	}
	return boot.Run(r, r, r, r, apiProbe{r}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRun_StartsServicesAfterTheMachineIsReady(t *testing.T) {
	r := &recorder{raw: validConfig}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, []config.Family{config.IPv4}, r.forwarded)
	require.Len(t, r.applied.Interfaces, 1)
	assert.Equal(t, "eth0", r.applied.Interfaces[0].Name)
	require.Len(t, r.written, 1)
	assert.Equal(t, "/run/mvmd/config.json", r.written[0].Path)
}

func TestRun_StartsAControlPlaneServiceOnlyOnAControlPlaneNode(t *testing.T) {
	r := &recorder{raw: replace(t, validConfig, "controlPlane: false", "controlPlane: true")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal", "start mvmd", "start gaon"}, r.calls)
}

// withPKI adds a spec.pki that asks for one certificate to validConfig.
func withPKI(t *testing.T) string {
	t.Helper()
	ca := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	return replace(t, validConfig, "  services:\n", `  pki:
    authorities:
      - name: ca
        certificate: |
`+pkitest.Indent(ca.Cert, 10)+`        key: |
`+pkitest.Indent(ca.Key, 10)+`        certificatePath: /run/pki/ca.pem
    certificates:
      - name: server
        authority: ca
        certificatePath: /run/pki/server.pem
        keyPath: /run/pki/server.key
        commonName: server
        usage: server
        nodeAddresses: true
  services:
`)
}

func TestRun_IssuesCertificatesAfterTheNetworkAndBeforeAnyServiceStarts(t *testing.T) {
	r := &recorder{raw: withPKI(t)}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "issue", "prepare", "seal", "start mvmd"}, r.calls)
	require.Len(t, r.issuedPKI.Certificates, 1)
	assert.Equal(t, "server", r.issuedPKI.Certificates[0].Name)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.78.0.1")}, r.issuedTo, "the Node's addresses come from the network-config")
}

// withTime adds a spec.time to doc, which holds the spec.services line.
func withTime(t *testing.T, doc string) string {
	t.Helper()
	return replace(t, doc, "  services:\n", "  time:\n    servers: [ntp.example, '192.0.2.1:1123']\n  services:\n")
}

func TestRun_SynchronisesTheClockAfterTheNetworkAndBeforeCertificatesAreIssued(t *testing.T) {
	r := &recorder{raw: withTime(t, withPKI(t))}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "time", "issue", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, []string{"ntp.example:123", "192.0.2.1:1123"}, r.timeServers, "each server is given with its port")
}

func TestRun_GoesOnWhenNoTimeServerAnswers(t *testing.T) {
	// The control is the test above: with the same configuration, the clock is set.
	r := &recorder{raw: withTime(t, withPKI(t)), timeErr: errors.New("no server answered")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "time", "issue", "prepare", "seal", "start mvmd"}, r.calls)
}

func withRoutes(t *testing.T, doc string) string {
	t.Helper()
	return replace(t, doc, "    forwarding: [ipv4]\n", "    forwarding: [ipv4]\n    routes:\n      - {to: 10.200.2.0/24, via: 10.78.0.2}\n      - {to: 10.200.3.0/24, via: 10.78.0.3}\n")
}

func TestRun_InstallsTheRoutesAfterForwardingAndBeforeFiles(t *testing.T) {
	r := &recorder{raw: withRoutes(t, validConfig)}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "route 10.200.2.0/24", "route 10.200.3.0/24", "files", "registries", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, netip.MustParseAddr("10.78.0.3"), r.routes[1].Via)
}

func TestRun_GoesOnWhenARouteCannotBeInstalled(t *testing.T) {
	// The control is the test above: with the same configuration every route is installed.
	r := &recorder{raw: withRoutes(t, validConfig), routeErr: errors.New("network is unreachable")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "route 10.200.2.0/24", "route 10.200.3.0/24", "files", "registries", "prepare", "seal", "start mvmd"}, r.calls, "the second route is tried and the services start")
}

func TestRun_SetsNoClockWithoutSpecTime(t *testing.T) {
	var out strings.Builder
	r := &recorder{raw: validConfig, network: validNetwork}

	_, err := boot.Run(r, r, r, r, apiProbe{r}, slog.New(slog.NewTextHandler(&out, nil)))

	require.NoError(t, err)
	assert.NotContains(t, r.calls, "time")
	assert.Contains(t, out.String(), "time is not synchronised")
}

func TestRun_StartsNothingWhenACertificateCannotBeIssued(t *testing.T) {
	// The control is the test above: with the same configuration, a service starts.
	r := &recorder{raw: withPKI(t), issueErr: errors.New("boom")}

	err := run(r)

	assert.ErrorIs(t, err, r.issueErr)
	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "issue"}, r.calls)
}

func TestRun_RefusesAnInvalidConfigurationAndStartsNothing(t *testing.T) {
	// The control is the valid configuration above: it starts a service.
	r := &recorder{raw: replace(t, validConfig, "binary: /usr/bin/mvmd", "binary: mvmd")}

	err := run(r)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.services[0].binary")
	assert.Equal(t, []string{"read"}, r.calls, "no network is brought up, no disk mounted and no service started")
}

func TestRun_RefusesAMalformedNetworkConfigAndStartsNothing(t *testing.T) {
	// The control is the valid network-config above: it brings the network up.
	r := &recorder{raw: validConfig, network: replace(t, validNetwork, "type: static", "type: dhcp4")}

	err := run(r)

	require.Error(t, err)
	assert.ErrorContains(t, err, "network-config")
	assert.Equal(t, []string{"read"}, r.calls, "nothing is applied: the network and the files wait for both documents to be valid")
}

func TestRun_StartsNothingWhenAStepBeforeTheServicesFails(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		r    *recorder
		want []string
	}{
		"configuration cannot be read": {&recorder{raw: validConfig, readErr: boom}, []string{"read"}},
		"network cannot be applied":    {&recorder{raw: validConfig, networkErr: boom}, []string{"read", "network"}},
		"data disk is refused":         {&recorder{raw: validConfig, diskErr: boom}, []string{"read", "network", "mount"}},
		"forwarding cannot be set":     {&recorder{raw: validConfig, forwardErr: boom}, []string{"read", "network", "mount", "load", "api", "forward"}},
		"files cannot be written":      {&recorder{raw: validConfig, filesErr: boom}, []string{"read", "network", "mount", "load", "api", "forward", "files"}},
		"cgroups cannot be prepared":   {&recorder{raw: validConfig, prepareErr: boom}, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare"}},
		"root cannot be sealed":        {&recorder{raw: validConfig, sealErr: boom}, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := run(tt.r)

			assert.ErrorIs(t, err, boom)
			assert.Equal(t, tt.want, tt.r.calls)
		})
	}
}

func TestRun_StartsTheNodeAPIWithTheChosenConfigurationBeforeAnythingElseCanRefuse(t *testing.T) {
	boom := errors.New("boom")
	r := &recorder{raw: validConfig, forwardErr: boom}

	err := run(r)

	require.ErrorIs(t, err, boom)
	require.NotNil(t, r.api, "the API started although a later step refused")
	assert.Equal(t, "node-1", r.api.Config.Metadata.Name)
	assert.Equal(t, 0, r.api.Revision)
}

func TestRun_RefusesWhenTheNodeAPICannotStart(t *testing.T) {
	boom := errors.New("boom")
	r := &recorder{raw: validConfig, apiErr: boom}

	err := run(r)

	assert.ErrorIs(t, err, boom)
	assert.ErrorContains(t, err, "start the Node API")
	assert.Equal(t, []string{"read", "network", "mount", "load", "api"}, r.calls)
}

func TestRun_ReportsAServiceThatCannotStart(t *testing.T) {
	r := &recorder{raw: validConfig, startErr: errors.New("boom")}

	err := run(r)

	assert.ErrorContains(t, err, "start mvmd")
}

func replace(t *testing.T, doc, old, new string) string {
	t.Helper()
	out := strings.Replace(doc, old, new, 1)
	require.NotEqual(t, doc, out, "the setup changed nothing")
	return out
}

func TestRun_WritesTheRegistryFilesBeforeAnyServiceStarts(t *testing.T) {
	doc := replace(t, validConfig, "  services:\n", "  registries:\n    - host: registry.example\n      credential: {helper: stub}\n  services:\n")
	r := &recorder{raw: doc}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal", "start mvmd"}, r.calls)
	require.Len(t, r.registries.Registries, 1)
	assert.Equal(t, "registry.example", r.registries.Registries[0].Host)
}

// withKeptPair gives the mvmd service of validConfig a kept pair.
func withKeptPair(t *testing.T) string {
	t.Helper()
	return replace(t, validConfig, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      keptPair: {file: /var/lib/mvmd/agent/node-agent.pem, organization: gaon:nodes}\n")
}

func TestRun_ChecksAServicesKeptPairJustBeforeItStarts(t *testing.T) {
	r := &recorder{raw: withKeptPair(t), setAside: true}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal", "pair /var/lib/mvmd/agent/node-agent.pem", "start mvmd"}, r.calls)
}

func TestRun_StartsTheServiceWhenItsKeptPairCannotBeSetAside(t *testing.T) {
	r := &recorder{raw: withKeptPair(t), setAsideErr: errors.New("read-only")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"mvmd"}, r.started)
}

func TestRun_RefusesWhenTheRegistryFilesCannotBeWritten(t *testing.T) {
	r := &recorder{raw: validConfig, registriesErr: errors.New("disk full")}

	require.Error(t, run(r))

	assert.Empty(t, r.started)
}
