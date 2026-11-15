package hardware_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/hardware"
	"github.com/garamsh/naru/lib/orchapi"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// machine is a tree shaped like /proc/meminfo, /sys and /dev: a blank disk, a disk with a partition,
// a disk with a filesystem and no partition table, a loop device and an optical drive that are not
// disks, and two links one of which has a carrier.
func machine(t *testing.T) hardware.Source {
	t.Helper()
	root := t.TempDir()
	s := hardware.Source{Meminfo: filepath.Join(root, "meminfo"), Sys: filepath.Join(root, "sys"), Dev: filepath.Join(root, "dev"), CPUs: 8}
	write(t, s.Meminfo, "MemTotal:       16384 kB\nMemFree:  1 kB\n")
	for name, c := range map[string]struct {
		sectors, model string
		dev            []byte
		partition      bool
	}{
		"vda":   {"2097152", "blank disk\n", make([]byte, 256<<10), false},
		"vdb":   {"4194304", "has partition\n", make([]byte, 256<<10), true},
		"vdc":   {"2097152", "has filesystem\n", append(make([]byte, 1080), 0x53, 0xEF), false},
		"loop0": {"8", "", make([]byte, 4096), false},
		"sr0":   {"8", "", make([]byte, 4096), false},
	} {
		write(t, filepath.Join(s.Sys, "block", name, "size"), c.sectors+"\n")
		if c.model != "" {
			write(t, filepath.Join(s.Sys, "block", name, "device", "model"), c.model)
		}
		if c.partition {
			write(t, filepath.Join(s.Sys, "block", name, name+"1", "partition"), "1\n")
		}
		require.NoError(t, os.MkdirAll(s.Dev, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(s.Dev, name), c.dev, 0o644))
	}
	write(t, filepath.Join(s.Sys, "class", "net", "lo", "address"), "00:00:00:00:00:00\n")
	write(t, filepath.Join(s.Sys, "class", "net", "eth0", "address"), "52:54:00:12:34:56\n")
	write(t, filepath.Join(s.Sys, "class", "net", "eth0", "carrier"), "1\n")
	write(t, filepath.Join(s.Sys, "class", "net", "eth1", "address"), "52:54:00:12:34:57\n")
	return s
}

func TestCollect_ReportsProcessorsMemoryDisksAndLinks(t *testing.T) {
	got, err := machine(t).Collect()
	require.NoError(t, err)
	assert.Equal(t, &orchapi.Hardware{
		CPUs: 8, MemoryBytes: 16384 << 10,
		Disks: []orchapi.Disk{
			{Name: "vda", Size: 2097152 * 512, Model: "blank disk", Blank: true},
			{Name: "vdb", Size: 4194304 * 512, Model: "has partition", Blank: false},
			{Name: "vdc", Size: 2097152 * 512, Model: "has filesystem", Blank: false},
		},
		Links: []orchapi.Link{
			{Name: "eth0", MAC: "52:54:00:12:34:56", Carrier: true},
			{Name: "eth1", MAC: "52:54:00:12:34:57", Carrier: false},
		},
	}, got, "loop and optical devices and lo are not reported")
}

func TestCollect_ADiskItCannotReadIsNotReportedBlank(t *testing.T) {
	s := machine(t)
	require.NoError(t, os.Remove(filepath.Join(s.Dev, "vda")))
	got, err := s.Collect()
	require.NoError(t, err)
	require.Len(t, got.Disks, 3)
	assert.False(t, got.Disks[0].Blank)
}

func TestCollect_FailsWithoutMemTotal(t *testing.T) {
	s := machine(t)
	write(t, s.Meminfo, "MemFree: 1 kB\n")
	_, err := s.Collect()
	assert.ErrorContains(t, err, "no MemTotal")
}
