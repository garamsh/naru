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
)

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
	readErr    error
	diskErr    error
	forwardErr error
	prepareErr error
	startErr   error

	calls     []string
	forwarded []config.Family
}

func (r *recorder) ReadConfig() ([]byte, error) {
	r.calls = append(r.calls, "read")
	return []byte(r.raw), r.readErr
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

func (r *recorder) Prepare() error {
	r.calls = append(r.calls, "prepare")
	return r.prepareErr
}

func (r *recorder) Start(s config.Service) error {
	r.calls = append(r.calls, "start "+s.Name)
	return r.startErr
}

func run(r *recorder) error {
	return boot.Run(r, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRun_StartsServicesAfterTheMachineIsReady(t *testing.T) {
	r := &recorder{raw: validConfig}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "mount", "forward", "prepare", "start mvmd"}, r.calls)
	assert.Equal(t, []config.Family{config.IPv4}, r.forwarded)
}

func TestRun_StartsAControlPlaneServiceOnlyOnAControlPlaneNode(t *testing.T) {
	r := &recorder{raw: replace(t, validConfig, "controlPlane: false", "controlPlane: true")}

	require.NoError(t, run(r))

	assert.Equal(t, []string{"read", "mount", "forward", "prepare", "start mvmd", "start gaon"}, r.calls)
}

func TestRun_RefusesAnInvalidConfigurationAndStartsNothing(t *testing.T) {
	// The control is the valid configuration above: it starts a service.
	r := &recorder{raw: replace(t, validConfig, "binary: /usr/bin/mvmd", "binary: mvmd")}

	err := run(r)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.services[0].binary")
	assert.Equal(t, []string{"read"}, r.calls, "no disk is mounted and no service started")
}

func TestRun_StartsNothingWhenAStepBeforeTheServicesFails(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		r    *recorder
		want []string
	}{
		"configuration cannot be read": {&recorder{raw: validConfig, readErr: boom}, []string{"read"}},
		"data disk is refused":         {&recorder{raw: validConfig, diskErr: boom}, []string{"read", "mount"}},
		"forwarding cannot be set":     {&recorder{raw: validConfig, forwardErr: boom}, []string{"read", "mount", "forward"}},
		"cgroups cannot be prepared":   {&recorder{raw: validConfig, prepareErr: boom}, []string{"read", "mount", "forward", "prepare"}},
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
