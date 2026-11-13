package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
)

// storageDoc is the valid document with spec.storage, each entry written as a flow mapping.
func storageDoc(entries ...string) string {
	var list strings.Builder
	list.WriteString("  storage:\n")
	for _, e := range entries {
		list.WriteString("    - " + e + "\n")
	}
	return strings.Replace(valid, "  dataDisk:\n", list.String()+"  dataDisk:\n", 1)
}

func TestParse_AcceptsSpecStorage(t *testing.T) {
	cfg, err := config.Parse([]byte(storageDoc(
		"{name: vmdisks, label: vmdisks}",
		"{name: scratch, device: /dev/sdc, form: xfs, mountPoint: /var/lib/naru/mvmd/disks}",
	)))

	require.NoError(t, err)
	require.Len(t, cfg.Spec.Storage, 2)
	assert.Equal(t, "/var/lib/naru/storage/vmdisks", cfg.Spec.Storage[0].Mount())
	assert.Equal(t, "/var/lib/naru/mvmd/disks", cfg.Spec.Storage[1].Mount())
}

func TestParse_AcceptsConfigurationWithoutSpecStorage(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))

	require.NoError(t, err)
	assert.Empty(t, cfg.Spec.Storage)
}

func TestParse_RefusesSpecStorageAndNamesTheField(t *testing.T) {
	tests := []struct {
		name  string
		doc   string
		field string
	}{
		{"no name", storageDoc("{label: vmdisks}"), "spec.storage[0].name"},
		{"a name longer than an XFS label", storageDoc("{name: thirteen-char, label: x}"), "spec.storage[0].name"},
		{"a name with a slash", storageDoc("{name: a/b, label: x}"), "spec.storage[0].name"},
		{"a name twice", storageDoc("{name: a, label: x}", "{name: a, label: y, mountPoint: /mnt/y}"), "spec.storage[1].name"},
		{"neither label nor device", storageDoc("{name: a}"), "spec.storage[0]"},
		{"both label and device", storageDoc("{name: a, label: x, device: /dev/sdb}"), "spec.storage[0]"},
		{"a relative device", storageDoc("{name: a, device: sdb}"), "spec.storage[0].device"},
		{"a form other than xfs", storageDoc("{name: a, label: x, form: ext4}"), "spec.storage[0].form"},
		{"a relative mount point", storageDoc("{name: a, label: x, mountPoint: disks}"), "spec.storage[0].mountPoint"},
		{"the data disk's mount point", storageDoc("{name: a, label: x, mountPoint: /var/lib/mvmd}"), "spec.storage[0].mountPoint"},
		{"a mount point twice", storageDoc("{name: a, label: x, mountPoint: /mnt/a}", "{name: b, label: y, mountPoint: /mnt/a}"), "spec.storage[1].mountPoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.doc))

			require.Error(t, err)
			assert.Equal(t, []string{tt.field}, fields(t, err))
		})
	}
}
