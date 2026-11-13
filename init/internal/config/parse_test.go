package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
)

const valid = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  controlPlane: true
  services:
    - name: mvmd
      binary: /usr/bin/mvmd
      args: ["--state-dir", "/var/lib/mvmd"]
      env:
        MVMD_LOG: debug
    - name: etcd
      binary: /usr/bin/etcd
      controlPlane: true
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/mvmd
  network:
    forwarding: [ipv4, ipv6]
  files:
    - path: /run/mvmd/config.json
      mode: "0600"
      content: |
        {"listen": ":8080"}
    - path: /run/mvmd/token
      content: secret
`

// fields returns the Field of every *FieldError joined in err.
func fields(t *testing.T, err error) []string {
	t.Helper()
	var out []string
	var walk func(error)
	walk = func(e error) {
		var fe *config.FieldError
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range j.Unwrap() {
				walk(c)
			}
		} else if errors.As(e, &fe) {
			out = append(out, fe.Field)
		}
	}
	walk(err)
	return out
}

func TestParse_Valid(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))
	require.NoError(t, err)

	assert.Equal(t, "node-1", cfg.Metadata.Name)
	require.Len(t, cfg.Spec.Services, 2)
	assert.Equal(t, "mvmd", cfg.Spec.Services[0].Name)
	assert.Equal(t, []string{"--state-dir", "/var/lib/mvmd"}, cfg.Spec.Services[0].Args)
	assert.Equal(t, map[string]string{"MVMD_LOG": "debug"}, cfg.Spec.Services[0].Env)
	assert.True(t, cfg.Spec.Services[1].ControlPlane)
	assert.Equal(t, "NARUDATA", cfg.Spec.DataDisk.Label)
	assert.Equal(t, []config.Family{config.IPv4, config.IPv6}, cfg.Spec.Network.Forwarding)
	require.Len(t, cfg.Spec.Files, 2)
	assert.Equal(t, "/run/mvmd/config.json", cfg.Spec.Files[0].Path)
	assert.Equal(t, "{\"listen\": \":8080\"}\n", cfg.Spec.Files[0].Content)
	assert.Equal(t, os.FileMode(0o600), cfg.Spec.Files[0].Perm())
	assert.Equal(t, os.FileMode(0o644), cfg.Spec.Files[1].Perm(), "a file without a mode is 0644")
}

func TestParse_RefusesAndNamesTheField(t *testing.T) {
	tests := []struct {
		name  string
		from  string
		to    string
		field string
	}{
		{"unknown apiVersion", "naru.garam.sh/v1alpha1", "naru.garam.sh/v2", "apiVersion"},
		{"unknown kind", "kind: NodeConfig", "kind: Other", "kind"},
		{"empty node name", "name: node-1", "name: ''", "metadata.name"},
		{"unknown top-level field", "spec:\n", "bogus: 1\nspec:\n", "bogus"},
		{"unknown nested field", "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      bogus: 1\n", "spec.services[0].bogus"},
		{"mistyped bool", "controlPlane: true\n  services", "controlPlane: maybe\n  services", "spec.controlPlane"},
		{"relative binary", "binary: /usr/bin/etcd", "binary: etcd", "spec.services[1].binary"},
		{"duplicate service name", "name: etcd", "name: mvmd", "spec.services[1].name"},
		{"reserved service name", "name: etcd", "name: detached", "spec.services[1].name"},
		{"delegated cgroup name", "name: etcd", "name: kubernetes", "spec.services[1].name"},
		{"invalid env name", "MVMD_LOG: debug", "'A=B': debug", "spec.services[0].env"},
		{"disk named twice", "label: NARUDATA", "label: NARUDATA\n    device: /dev/vdb", "spec.dataDisk"},
		{"disk not named", "    label: NARUDATA\n", "", "spec.dataDisk"},
		{"label too long", "NARUDATA", "ABCDEFGHIJKLMNOPQ", "spec.dataDisk.label"},
		{"relative mount point", "mountPoint: /var/lib/mvmd", "mountPoint: var/lib", "spec.dataDisk.mountPoint"},
		{"unknown family", "[ipv4, ipv6]", "[ipv4, ipx]", "spec.network.forwarding[1]"},
		{"repeated family", "[ipv4, ipv6]", "[ipv4, ipv4]", "spec.network.forwarding[1]"},
		{"file outside /run", "path: /run/mvmd/token", "path: /etc/token", "spec.files[1].path"},
		{"file path not clean", "path: /run/mvmd/token", "path: /run/mvmd/../token", "spec.files[1].path"},
		{"file in init's directory", "path: /run/mvmd/token", "path: /run/naru/resolv.conf", "spec.files[1].path"},
		{"file that is init's directory", "path: /run/mvmd/token", "path: /run/naru", "spec.files[1].path"},
		{"file mode not octal", `mode: "0600"`, `mode: "rw"`, "spec.files[0].mode"},
		{"file mode with a setuid bit", `mode: "0600"`, `mode: "4755"`, "spec.files[0].mode"},
		{"file path used twice", "path: /run/mvmd/token", "path: /run/mvmd/config.json", "spec.files[1].path"},
		{"file path inside another file", "path: /run/mvmd/token", "path: /run/mvmd/config.json/x", "spec.files[1].path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, valid, tt.from, "the case must change the valid document")
			doc := strings.Replace(valid, tt.from, tt.to, 1)

			_, err := config.Parse([]byte(doc))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
			assert.Contains(t, err.Error(), tt.field)
		})
	}
}

func TestParse_RefusesUnparsableFile(t *testing.T) {
	_, err := config.Parse([]byte("spec: ["))

	require.Error(t, err)
	assert.Empty(t, fields(t, err), "a syntax error concerns no field")
}

func TestParse_ReportsEveryInvalidField(t *testing.T) {
	doc := strings.NewReplacer("kind: NodeConfig", "kind: Other", "mountPoint: /var/lib/mvmd", "mountPoint: x").Replace(valid)

	_, err := config.Parse([]byte(doc))

	assert.ElementsMatch(t, []string{"kind", "spec.dataDisk.mountPoint"}, fields(t, err))
}
