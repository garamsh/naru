package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// fakeNaructl records the arguments of gen node and writes a user-data with no services and an empty
// network-config where it is told to.
func fakeNaructl(t *testing.T) (tool naructl.Tool, args string) {
	t.Helper()
	dir := t.TempDir()
	args = filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$*" > ` + args + `
while [ $# -gt 0 ]; do
  case "$1" in
    --out) printf 'spec:\n  services: []\n' > "$2" ;;
    --network-config) : > "$2" ;;
  esac
  shift
done
`
	bin := filepath.Join(dir, "naructl")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return naructl.Tool{Bin: bin}, args
}

func genNode(t *testing.T, c Config) (userData []byte, line string, err error) {
	t.Helper()
	tool, args := fakeNaructl(t)
	spec := ClusterSpec{N: 1, GenNode: true, Dir: t.TempDir(), Naructl: tool}
	c.Name = "node-1"
	userData, _, err = (&genFiles{caDir: t.TempDir()}).node(c, spec)
	b, _ := os.ReadFile(args)
	return userData, string(b), err
}

func TestGenNode_StorageIsPassedToNaructlAndItsProbeIsAnAppendedService(t *testing.T) {
	c := cfg(1, 1, false)
	c.Storage = []Storage{{Name: "vmdisks", Device: "/dev/vdb", MountPoint: wire.MvmdStorageMount}}
	c.StorageProbe = wire.MvmdStorageMount

	userData, line, err := genNode(t, c)

	require.NoError(t, err)
	assert.Contains(t, line, "--storage vmdisks=/dev/vdb")
	var doc struct {
		Spec struct {
			Services []service `yaml:"services"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(userData, &doc))
	require.Len(t, doc.Spec.Services, 1)
	assert.Equal(t, storageProbeService(wire.MvmdStorageMount), doc.Spec.Services[0])
	assert.Equal(t, []string{"-dir", wire.MvmdStorageMount}, doc.Spec.Services[0].Args)
}

func TestGenNode_WithoutStorageNaructlIsToldOfNoneAndNoProbeIsAdded(t *testing.T) {
	userData, line, err := genNode(t, cfg(1, 1, false))

	require.NoError(t, err)
	assert.NotContains(t, line, "--storage")
	assert.False(t, strings.Contains(string(userData), "storageprobe"))
}

func TestGenNode_AFirstDiskMountedWhereNaructlDoesNotMountItIsRefused(t *testing.T) {
	c := cfg(1, 1, false)
	c.Storage = []Storage{{Name: "vmdisks", Device: "/dev/vdb", MountPoint: "/mnt/elsewhere"}}

	_, _, err := genNode(t, c)

	require.ErrorContains(t, err, wire.MvmdStorageMount)
}
