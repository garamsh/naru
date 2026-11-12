package naructl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// printing is an executable that prints out for `topology` and fails for anything else.
func printing(t *testing.T, out string) naructl.Tool {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = topology ] || exit 2\ncat <<'EOF'\n"+out+"\nEOF\n"), 0o755)) //nolint:gosec // a test executable
	return naructl.Tool{Bin: bin}
}

const sample = `{"services":[{"name":"registry","controlPlane":false,"gaon":false},
{"name":"etcd","controlPlane":true,"gaon":false},{"name":"gaon-apiserver","controlPlane":true,"gaon":true},
{"name":"gaon-node-agent","controlPlane":false,"gaon":true}],"signer":"gaon-apiserver","apiserver":"gaon-apiserver","agent":"gaon-node-agent"}`

func TestTopology_IsWhatNaructlPrintsAndAnswersWhatEachNodeRuns(t *testing.T) {
	top, err := printing(t, sample).Topology()

	require.NoError(t, err)
	assert.Equal(t, []string{"registry", "etcd", "gaon-apiserver", "gaon-node-agent"}, top.Names())
	assert.Equal(t, []string{"gaon-apiserver", "gaon-node-agent"}, top.GaonNames())
	assert.Equal(t, "gaon-apiserver", top.Signer)
	assert.True(t, top.Has("etcd"))
	assert.False(t, top.Has("gaon-scheduler"))
	var worker, controlPlane []string
	for _, s := range top.Running(false) {
		worker = append(worker, s.Name)
	}
	for _, s := range top.Running(true) {
		controlPlane = append(controlPlane, s.Name)
	}
	assert.Equal(t, []string{"registry", "gaon-node-agent"}, worker)
	assert.Equal(t, top.Names(), controlPlane)
	assert.ErrorContains(t, top.CheckNames([]string{"registry", "nothing"}), `"nothing" is not a service; the names are registry, etcd, gaon-apiserver, gaon-node-agent`)
}

func TestTopology_ANaructlWithoutTheCommandOrWithAnEmptyAnswerIsAnErrorThatSaysSo(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho usage >&2\nexit 1\n"), 0o755)) //nolint:gosec // a test executable

	_, err := naructl.Tool{Bin: bin}.Topology()
	assert.ErrorContains(t, err, "has no topology command")
	assert.ErrorContains(t, err, "usage")

	_, err = printing(t, `{}`).Topology()
	assert.ErrorContains(t, err, "no services or no signer, API server or agent")

	_, err = printing(t, `not json`).Topology()
	assert.ErrorContains(t, err, "naructl topology")

	_, err = naructl.Tool{}.Topology()
	assert.ErrorContains(t, err, "no naructl binary")
}

// TestTopology_OfTheNaructlBuiltFromTheCheckoutIsTheServicesItsUserDataRuns is the chain from the
// layout to the harness: whatever the layout's user-data runs, the topology the harness reads names,
// in the same order, with the same control-plane marks, so a service added or renamed in the layout
// is seen here without a change to the harness.
func TestTopology_OfTheNaructlBuiltFromTheCheckoutIsTheServicesItsUserDataRuns(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "naructl")
	require.NoError(t, naructl.Build(repoRoot(t), bin))
	tool := naructl.Tool{Bin: bin}
	top, err := tool.Topology()
	require.NoError(t, err)

	ca := filepath.Join(root, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenToken(ca, "node-1"))
	d := func(c string) string { return strings.Repeat(c, 64) }
	guest := filepath.Join(root, "guest.json")
	require.NoError(t, os.WriteFile(guest, []byte(`{"kernels":{"naru":{"path":"/k","sha256":"`+d("a")+`"}},"default_kernel":"naru",
"guest_initrd":{"path":"/g","sha256":"`+d("c")+`"},"converter_initrd":{"path":"/c","sha256":"`+d("d")+`"}}`), 0o600))
	out := filepath.Join(root, "node-1.yaml")
	require.NoError(t, tool.GenNode(naructl.Node{
		CA: ca, Name: "node-1", ControlPlane: true, Address: "10.78.0.1/24", APIServer: "10.78.0.1",
		MachinePrefix: "10.200.1.0/24", Guest: guest, Firecracker: d("7"), Out: out,
	}))
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	var c struct {
		Spec struct {
			Services []struct {
				Name         string `yaml:"name"`
				ControlPlane bool   `yaml:"controlPlane"`
			} `yaml:"services"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(b, &c))

	var got []string
	marks := map[string]bool{}
	for _, s := range c.Spec.Services {
		if s.Name == "mvmd" {
			continue
		}
		got = append(got, s.Name)
		marks[s.Name] = s.ControlPlane
	}
	assert.Equal(t, got, top.Names())
	for _, s := range top.Services {
		assert.Equal(t, s.ControlPlane, marks[s.Name], s.Name)
	}
	assert.True(t, top.Has(top.Signer), "the signer is one of the services")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.work above the test")
		dir = parent
	}
}
