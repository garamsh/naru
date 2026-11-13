package command_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

type storedDisk struct {
	Name       string
	Label      string
	Device     string
	MountPoint string `yaml:"mountPoint"`
}

func storage(t *testing.T, path string) []storedDisk {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var u struct {
		Spec struct{ Storage []storedDisk }
	}
	require.NoError(t, yaml.Unmarshal(b, &u))
	return u.Spec.Storage
}

func TestGenNode_StorageIsWrittenAndTheFirstDiskIsMountedWhereMvmdKeepsItsDisks(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--storage", "vmdisks=vmdisks", "--storage", "scratch=extra")

	assert.Equal(t, []storedDisk{
		{"vmdisks", "vmdisks", "", "/var/lib/naru/mvmd/disks"},
		{"scratch", "extra", "", ""},
	}, storage(t, path))
	validate(t, path) // naru-init accepts spec.storage
}

func TestGenNode_AValueStartingWithDevIsTheDeviceAndAnyOtherIsTheLabel(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--storage", "vmdisks=/dev/sdc", "--storage", "scratch=extra")

	assert.Equal(t, []storedDisk{
		{"vmdisks", "", "/dev/sdc", "/var/lib/naru/mvmd/disks"},
		{"scratch", "extra", "", ""},
	}, storage(t, path))
	validate(t, path) // naru-init accepts a device entry
}

func TestGenNode_WithoutStorageTheUserDataCarriesNone(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1")

	assert.Empty(t, storage(t, path))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "storage")
}

func TestGenNode_RefusesStorageInitWouldRefuse(t *testing.T) {
	dep := newDeployment(t)
	tests := []struct {
		name    string
		storage []string
		reason  string
	}{
		{"not <name>=<value>", []string{"vmdisks"}, "is not <name>=<label or /dev/path>"},
		{"no label", []string{"vmdisks="}, "has no label or device"},
		{"a name too long for an XFS label", []string{"averylongname1=x"}, "at most 12"},
		{"a name that is no DNS label", []string{"Vm_Disks=x"}, "DNS label"},
		{"a name twice", []string{"a=x", "a=y"}, "given twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
				"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
			}
			for _, s := range tt.storage {
				args = append(args, "--storage", s)
			}

			out, err := run(t, args...)

			require.Error(t, err)
			assert.Contains(t, out, tt.reason)
		})
	}
}
