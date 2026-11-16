package node

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// virtioDevices is the virtio-blk -device values of the data and extra disks, in command-line order.
func virtioDevices(t *testing.T, mutate func(*Spec)) []string {
	t.Helper()
	dir := t.TempDir()
	boot := dir + "/naru.img"
	require.NoError(t, os.WriteFile(boot, nil, 0o644))
	spec := Spec{Config: cfg(1, 2, true, false), Disk: boot, Dir: dir, Mcast: "230.78.0.1:5000"}
	spec.Config.ExtraDisk = 512 << 20
	mutate(&spec)
	args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	var devices []string
	for i, a := range args {
		if a == "-device" && (args[i+1] == "virtio-blk-pci,drive=data" || args[i+1] == "virtio-blk-pci,drive=extra") {
			devices = append(devices, args[i+1])
		}
	}
	return devices
}

func TestQemuArgs_VirtioDisksAreAttachedInTheOrderTheSpecGives(t *testing.T) {
	data, extra := "virtio-blk-pci,drive=data", "virtio-blk-pci,drive=extra"

	first := virtioDevices(t, func(s *Spec) { s.Config.VirtioDisks = true })
	swapped := virtioDevices(t, func(s *Spec) { s.Config.VirtioDisks = true; s.ExtraDiskFirst = true })
	scsi := virtioDevices(t, func(s *Spec) {})

	assert.Equal(t, []string{data, extra}, first)
	assert.Equal(t, []string{extra, data}, swapped)
	assert.Empty(t, scsi, "the default layout keeps both disks on the SCSI controller")
}

func TestQemuArgs_TheExtraDiskCarriesItsSerialNumberAndTheDataDiskDoesNot(t *testing.T) {
	for _, tt := range []struct {
		name   string
		virtio bool
		extra  string
		data   string
	}{
		{"virtio-blk", true, "virtio-blk-pci,drive=extra,serial=S3RIAL-01", "virtio-blk-pci,drive=data"},
		{"scsi-hd", false, "scsi-hd,bus=scsihw0.0,drive=extra,serial=S3RIAL-01", "scsi-hd,bus=scsihw0.0,drive=data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			boot := dir + "/naru.img"
			require.NoError(t, os.WriteFile(boot, nil, 0o644))
			spec := Spec{Config: cfg(1, 2, true, false), Disk: boot, Dir: dir, Mcast: "230.78.0.1:5000"}
			spec.Config.ExtraDisk, spec.Config.VirtioDisks, spec.Config.ExtraDiskSerial = 512<<20, tt.virtio, "S3RIAL-01"

			args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")

			require.NoError(t, err)
			assert.Contains(t, args, tt.extra)
			assert.Contains(t, args, tt.data)
		})
	}
}
