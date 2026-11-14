package node

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_StorageAndItsProbeAreInTheUserData(t *testing.T) {
	c := cfg(1, 2, true, false)
	c.Storage = []Storage{{Name: "vmdisks", Device: "/dev/sdc", MountPoint: "/var/lib/naru/mvmd/disks"}}
	c.StorageProbe = "/var/lib/naru/mvmd/disks"

	n := parse(t, c)

	assert.Equal(t, []storage{{Name: "vmdisks", Device: "/dev/sdc", MountPoint: "/var/lib/naru/mvmd/disks"}}, n.Spec.Storage)
	assert.Contains(t, names(n), "storageprobe")
}

func TestConfig_WithoutStorageTheUserDataHasNone(t *testing.T) {
	n := parse(t, cfg(1, 2, true, false))

	assert.Empty(t, n.Spec.Storage)
	assert.NotContains(t, names(n), "storageprobe")
}

func TestQemuArgs_AnExtraDiskIsADriveOnTheSCSIControllerAndNoneWithoutIt(t *testing.T) {
	dir := t.TempDir()
	boot := dir + "/naru.img"
	require.NoError(t, os.WriteFile(boot, nil, 0o644))
	spec := Spec{Config: cfg(1, 2, true, false), Disk: boot, Dir: dir, Mcast: "230.78.0.1:5000"}

	without, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	spec.Config.ExtraDisk = 512 << 20
	with, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)

	assert.NotContains(t, without, "scsi-hd,bus=scsihw0.0,drive=extra")
	assert.Contains(t, with, "scsi-hd,bus=scsihw0.0,drive=extra")
	assert.Contains(t, with, "if=none,id=extra,format=raw,file="+dir+"/extra.img")
}

func TestInitramfsList_InstallsMkfsXFSForStorageDisks(t *testing.T) {
	list, err := os.ReadFile("../../../../image/host/initramfs/initramfs.list")
	require.NoError(t, err)

	assert.Regexp(t, `(?m)^file `+regexp.QuoteMeta("/bin/mkfs.xfs")+` @OUT@/mkfs\.xfs 0755 `, string(list))
}
