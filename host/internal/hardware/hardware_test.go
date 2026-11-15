package hardware_test

import (
	"fmt"
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

// machine is a tree shaped like /proc/meminfo and /sys: a disk with no partition, a disk with two,
// another with none, a loop device and an optical drive that are not
// disks, and two links one of which has a carrier.
func machine(t *testing.T) hardware.Source {
	t.Helper()
	root := t.TempDir()
	s := hardware.Source{Meminfo: filepath.Join(root, "meminfo"), Sys: filepath.Join(root, "sys"), CPUs: 8}
	write(t, s.Meminfo, "MemTotal:       16384 kB\nMemFree:  1 kB\n")
	for name, c := range map[string]struct {
		sectors, model string
		partitions     int
	}{
		"vda":   {"2097152", "no partition\n", 0},
		"vdb":   {"4194304", "has partitions\n", 2},
		"vdc":   {"2097152", "no partition either\n", 0},
		"loop0": {"8", "", 0},
		"sr0":   {"8", "", 0},
	} {
		write(t, filepath.Join(s.Sys, "block", name, "size"), c.sectors+"\n")
		if c.model != "" {
			write(t, filepath.Join(s.Sys, "block", name, "device", "model"), c.model)
		}
		for i := 1; i <= c.partitions; i++ {
			write(t, filepath.Join(s.Sys, "block", name, fmt.Sprintf("%s%d", name, i), "partition"), fmt.Sprintf("%d\n", i))
		}
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
			{Name: "vda", Size: 2097152 * 512, Model: "no partition"},
			{Name: "vdb", Size: 4194304 * 512, Model: "has partitions", Partitions: 2},
			{Name: "vdc", Size: 2097152 * 512, Model: "no partition either"},
		},
		Links: []orchapi.Link{
			{Name: "eth0", MAC: "52:54:00:12:34:56", Carrier: true},
			{Name: "eth1", MAC: "52:54:00:12:34:57", Carrier: false},
		},
	}, got, "loop and optical devices and lo are not reported")
}

func TestCollect_FailsWithoutMemTotal(t *testing.T) {
	s := machine(t)
	write(t, s.Meminfo, "MemFree: 1 kB\n")
	_, err := s.Collect()
	assert.ErrorContains(t, err, "no MemTotal")
}
