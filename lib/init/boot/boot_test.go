package boot_test

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
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
`

// recorder is both Host and Services and records the order of their calls.
type recorder struct {
	raw        string
	network    string
	readErr    error
	networkErr error
	filesErr   error
	diskErr    error
	formatErr  error
	forwardErr error
	prepareErr error
	sealErr    error
	timeErr    error
	routeErr   error
	storageErr error
	extErr     error
	startErr   error
	apiErr     error
	api        *store.Stored

	stored  *store.Stored
	loadErr error
	// storedNetwork is the network-config the store holds, when hasNetwork; networkSaved is what
	// SaveNetwork was given, when savedNetwork.
	storedNetwork []byte
	hasNetwork    bool
	networkSaved  []byte
	savedNetwork  bool
	saved         []store.Stored
	mounted       config.DataDisk
	// dataMounted is whether the data disk is mounted; the store reads that the recorder's
	// LoadAnchor and LoadNetwork refuse as an unmounted disk does.
	dataMounted bool
	started     []string

	calls     []string
	forwarded []config.Family
	applied   netcfg.Config
	leased    *netcfg.Applied
	formatted config.DataDisk
	written   []config.File

	timeServers   []string
	routes        []config.Route
	registries    config.Spec
	registriesErr error
	storage       []config.Storage
	extensions    []config.Extension

	// resetScope is the reset waiting, when resetWaiting. discarded, cleared and wiped record what
	// was done of it.
	resetScope   string
	resetWaiting bool
	resetErr     error
	discarded    bool
	cleared      bool
	wiped        *config.DataDisk
	clearErr     error
	wipeErr      error
	// anchor is the stored maintenance anchor, when hasAnchor; anchorSaved is what SaveAnchor was given.
	anchor      []byte
	hasAnchor   bool
	anchorSaved []byte
	appliedAll  []netcfg.Config
	wrote       []config.File
	startedSvc  []config.Service
}

func (r *recorder) Discard() error {
	r.calls = append(r.calls, "discard")
	r.discarded = true
	return nil
}

func (r *recorder) TakeReset() (string, bool, error) {
	scope, ok := r.resetScope, r.resetWaiting
	r.resetWaiting = false
	return scope, ok, r.resetErr
}

func (r *recorder) ClearDataDisk() error {
	r.calls = append(r.calls, "clear")
	r.cleared = true
	return r.clearErr
}

func (r *recorder) WipeDataDisk(d config.DataDisk) error {
	r.calls = append(r.calls, "wipe")
	r.wiped = &d
	r.dataMounted = false
	return r.wipeErr
}

func (r *recorder) LoadAnchor() ([]byte, error) {
	if !r.dataMounted || !r.hasAnchor {
		return nil, store.ErrNotFound
	}
	return r.anchor, nil
}

func (r *recorder) SaveAnchor(b []byte) error {
	r.anchorSaved = b
	return nil
}

func (r *recorder) MountExtensions(e []config.Extension) error {
	r.calls = append(r.calls, "extensions")
	r.extensions = e
	return r.extErr
}

func (r *recorder) ReadSeed() ([]byte, []byte, error) {
	r.calls = append(r.calls, "read")
	return []byte(r.raw), []byte(r.network), r.readErr
}

// ApplyNetwork gives the Node the addresses of the configuration, or r.leased when it is set, as a
// DHCPv4 lease would.
func (r *recorder) ApplyNetwork(c netcfg.Config, _ *slog.Logger) (netcfg.Applied, error) {
	r.calls = append(r.calls, "network")
	r.applied = c
	r.appliedAll = append(r.appliedAll, c)
	if r.leased != nil {
		return *r.leased, r.networkErr
	}
	return netcfg.Applied{}, r.networkErr
}

func (r *recorder) WriteFiles(f []config.File) error {
	r.calls = append(r.calls, "files")
	r.written = f
	r.wrote = append(r.wrote, f...)
	return r.filesErr
}

func (r *recorder) FormatBlankDataDisk(d config.DataDisk, _ *slog.Logger) error {
	r.calls = append(r.calls, "format")
	r.formatted = d
	return r.formatErr
}

func (r *recorder) MountDataDisk(d config.DataDisk) error {
	r.calls = append(r.calls, "mount")
	r.mounted = d
	r.dataMounted = r.diskErr == nil
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

func (r *recorder) PrepareStorage(s config.Storage, _ *slog.Logger) error {
	r.calls = append(r.calls, "storage "+s.Name)
	r.storage = append(r.storage, s)
	return r.storageErr
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

func (r *recorder) Prepare() error {
	r.calls = append(r.calls, "prepare")
	return r.prepareErr
}

func (r *recorder) Start(s config.Service) error {
	r.calls = append(r.calls, "start "+s.Name)
	r.started = append(r.started, s.Name)
	r.startedSvc = append(r.startedSvc, s)
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

func (r *recorder) LoadNetwork() ([]byte, error) {
	if !r.dataMounted || !r.hasNetwork {
		return nil, store.ErrNotFound
	}
	return r.storedNetwork, nil
}

func (r *recorder) SaveNetwork(b []byte) error {
	r.networkSaved, r.savedNetwork = b, true
	return nil
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
	return boot.Run(r, r, r, apiProbe{r}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// withTime adds a spec.time to doc, which holds the spec.services line.
func withTime(t *testing.T, doc string) string {
	t.Helper()
	return replace(t, doc, "  services:\n", "  time:\n    servers: [ntp.example, '192.0.2.1:1123']\n  services:\n")
}

func TestRun_SynchronisesTheClockAfterTheNetworkAndBeforeAnyServiceStarts(t *testing.T) {
	r := &recorder{raw: withTime(t, validConfig)}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "time", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, []string{"ntp.example:123", "192.0.2.1:1123"}, r.timeServers, "each server is given with its port")
}

func TestRun_GoesOnWhenNoTimeServerAnswers(t *testing.T) {
	// The control is the test above: with the same configuration, the clock is set.
	r := &recorder{raw: withTime(t, validConfig), timeErr: errors.New("no server answered")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "time", "prepare", "seal", "start mvmd"}, r.calls)
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

func withStorage(t *testing.T, doc string) string {
	t.Helper()
	return replace(t, doc, "  controlPlane: false\n", "  controlPlane: false\n  storage:\n    - {name: vmdisks, label: vmdisks}\n    - {name: scratch, device: /dev/sdc}\n")
}

func TestRun_PreparesTheStorageDisksAfterTheFilesAndBeforeAnyService(t *testing.T) {
	r := &recorder{raw: withStorage(t, validConfig)}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "storage vmdisks", "storage scratch", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, "/dev/sdc", r.storage[1].Device)
}

func TestRun_GoesOnWhenAStorageDiskCannotBePrepared(t *testing.T) {
	// The control is the test above: with the same configuration every disk is prepared.
	var out strings.Builder
	r := &recorder{raw: withStorage(t, validConfig), network: validNetwork, storageErr: errors.New("no disk with that label")}

	_, err := boot.Run(r, r, r, apiProbe{r}, slog.New(slog.NewTextHandler(&out, nil)))

	require.NoError(t, err)
	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "storage vmdisks", "storage scratch", "prepare", "seal", "start mvmd"}, r.calls, "the second disk is tried and the services start")
	assert.Contains(t, out.String(), "level=ERROR")
	assert.Contains(t, out.String(), "no disk with that label")
}

func TestRun_SetsNoClockWithoutSpecTime(t *testing.T) {
	var out strings.Builder
	r := &recorder{raw: validConfig, network: validNetwork}

	_, err := boot.Run(r, r, r, apiProbe{r}, slog.New(slog.NewTextHandler(&out, nil)))

	require.NoError(t, err)
	assert.NotContains(t, r.calls, "time")
	assert.Contains(t, out.String(), "time is not synchronised")
}

func TestRun_RefusesAnInvalidConfigurationAndStartsNothing(t *testing.T) {
	// The control is the valid configuration above: it starts a service.
	r := &recorder{raw: replace(t, validConfig, "binary: /usr/bin/mvmd", "binary: mvmd")}

	err := run(r)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.services[0].binary")
	assert.Equal(t, []string{"read", "mount"}, r.calls, "no network is brought up and no service started; the mount is maintenance mode's, to read the stored anchor")
}

func TestRun_RefusesAMalformedNetworkConfigAndStartsNothing(t *testing.T) {
	// The control is the valid network-config above: it brings the network up.
	r := &recorder{raw: validConfig, network: replace(t, validNetwork, "type: static", "type: dhcp4")}

	err := run(r)

	require.Error(t, err)
	assert.ErrorContains(t, err, "network-config")
	assert.Equal(t, []string{"read", "mount"}, r.calls, "nothing is applied: the network and the files wait for both documents to be valid; the mount is maintenance mode's")
}

func TestRun_StartsNothingWhenAStepBeforeTheServicesFails(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		r    *recorder
		want []string
	}{
		"configuration cannot be read": {&recorder{raw: validConfig, readErr: boom}, []string{"read", "mount"}},
		"network cannot be applied":    {&recorder{raw: validConfig, networkErr: boom}, []string{"read", "network", "mount"}},
		"data disk is refused":         {&recorder{raw: validConfig, diskErr: boom}, []string{"read", "network", "mount", "mount"}},
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

func TestRun_RefusesWhenTheRegistryFilesCannotBeWritten(t *testing.T) {
	r := &recorder{raw: validConfig, registriesErr: errors.New("disk full")}

	require.Error(t, run(r))

	assert.Empty(t, r.started)
}

func TestRun_FormatsTheBlankDataDiskOnlyWhenTheConfigurationAsksAndBeforeMountingIt(t *testing.T) {
	asks := replace(t, validConfig, "    label: NARUDATA\n", "    label: NARUDATA\n    formatBlank: true\n")

	r := &recorder{raw: asks}
	require.NoError(t, run(r))
	assert.Equal(t, []string{"read", "network", "format", "mount"}, r.calls[:4])

	r = &recorder{raw: validConfig}
	require.NoError(t, run(r))
	assert.NotContains(t, r.calls, "format", "a configuration that does not ask never formats")

	boom := errors.New("boom")
	r = &recorder{raw: asks, formatErr: boom}
	assert.ErrorIs(t, run(r), boom)
	assert.Equal(t, []string{"read", "network", "format", "mount"}, r.calls, "a disk that cannot be formatted is not mounted by the boot, only by maintenance mode, and nothing starts")
}

// withExtension adds a spec.extensions entry to validConfig.
func withExtension(t *testing.T) string {
	t.Helper()
	return replace(t, validConfig, "  services:\n", "  extensions:\n    - name: naru-orch\n      sha256: "+strings.Repeat("ab", 32)+"\n  services:\n")
}

func TestRun_MountsTheExtensionsBeforeTheRootIsSealedAndAnyServiceStarts(t *testing.T) {
	r := &recorder{raw: withExtension(t)}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "extensions", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, []config.Extension{{Name: "naru-orch", SHA256: strings.Repeat("ab", 32)}}, r.extensions)
}

func TestRun_StartsNothingWhenAnExtensionCannotBeMounted(t *testing.T) {
	// The control is the test above: with the same configuration, a service starts.
	r := &recorder{raw: withExtension(t), extErr: errors.New("no image is stored")}

	err := run(r)

	require.ErrorContains(t, err, "mount the system extensions: no image is stored")
	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "extensions"}, r.calls, "the API has started, so the image can still be delivered")
}

func TestRun_SynchronisesTheClockWithTheLeasesTimeServersWhenThereIsNoSpecTime(t *testing.T) {
	leased := netcfg.Applied{NTP: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}

	r := &recorder{raw: validConfig, leased: &leased}
	require.NoError(t, run(r))
	assert.Equal(t, []string{"192.0.2.1:123"}, r.timeServers)

	// The control: spec.time wins over the lease.
	r = &recorder{raw: withTime(t, validConfig), leased: &leased}
	require.NoError(t, run(r))
	assert.Equal(t, []string{"ntp.example:123", "192.0.2.1:1123"}, r.timeServers)

	// The control: with neither, no clock is set.
	r = &recorder{raw: validConfig}
	require.NoError(t, run(r))
	assert.NotContains(t, r.calls, "time")
}

func TestRun_FormatsTheFirstBlankDiskForAJoinForm(t *testing.T) {
	join := `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
spec:
  join:
    orch:
      url: https://orch.example.test:7500
      caSHA256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    enrollKey: naru-ek-fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`
	r := &recorder{raw: join}
	require.NoError(t, run(r))
	assert.True(t, r.formatted.FirstBlank)
	assert.Equal(t, "NARUDATA", r.formatted.Label)
	assert.Equal(t, []string{"naru-hostd"}, r.started)

	// The control: a configuration that asks for formatBlank itself does not take the first disk.
	r = &recorder{raw: replace(t, validConfig, "    label: NARUDATA\n", "    label: NARUDATA\n    formatBlank: true\n")}
	require.NoError(t, run(r))
	assert.False(t, r.formatted.FirstBlank)
}
