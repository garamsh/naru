package boot_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/boot"
	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/netcfg"
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
	startErr   error

	calls     []string
	forwarded []config.Family
	applied   netcfg.Config
	written   []config.File
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

func (r *recorder) MountDataDisk(config.DataDisk) error {
	r.calls = append(r.calls, "mount")
	return r.diskErr
}

func (r *recorder) EnableForwarding(f []config.Family) error {
	r.calls = append(r.calls, "forward")
	r.forwarded = f
	return r.forwardErr
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
	return r.startErr
}

func run(r *recorder) error {
	if r.network == "" {
		r.network = validNetwork
	}
	return boot.Run(r, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRun_StartsServicesAfterTheMachineIsReady(t *testing.T) {
	r := &recorder{raw: validConfig}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "forward", "files", "prepare", "seal", "start mvmd"}, r.calls)
	assert.Equal(t, []config.Family{config.IPv4}, r.forwarded)
	require.Len(t, r.applied.Interfaces, 1)
	assert.Equal(t, "eth0", r.applied.Interfaces[0].Name)
	require.Len(t, r.written, 1)
	assert.Equal(t, "/run/mvmd/config.json", r.written[0].Path)
}

func TestRun_StartsAControlPlaneServiceOnlyOnAControlPlaneNode(t *testing.T) {
	r := &recorder{raw: replace(t, validConfig, "controlPlane: false", "controlPlane: true")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "network", "mount", "forward", "files", "prepare", "seal", "start mvmd", "start gaon"}, r.calls)
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
		"forwarding cannot be set":     {&recorder{raw: validConfig, forwardErr: boom}, []string{"read", "network", "mount", "forward"}},
		"files cannot be written":      {&recorder{raw: validConfig, filesErr: boom}, []string{"read", "network", "mount", "forward", "files"}},
		"cgroups cannot be prepared":   {&recorder{raw: validConfig, prepareErr: boom}, []string{"read", "network", "mount", "forward", "files", "prepare"}},
		"root cannot be sealed":        {&recorder{raw: validConfig, sealErr: boom}, []string{"read", "network", "mount", "forward", "files", "prepare", "seal"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := run(tt.r)

			assert.ErrorIs(t, err, boom)
			assert.Equal(t, tt.want, tt.r.calls)
		})
	}
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
