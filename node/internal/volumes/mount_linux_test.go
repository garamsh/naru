package volumes

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

const flagged = `22 1 8:1 / / rw - ext4 /dev/root rw
40 22 253:16 / /var/lib/naru/storage/locked rw,nosuid,nodev,noexec,relatime - xfs /dev/vdb rw,prjquota
41 22 253:32 / /var/lib/naru/storage/plain rw,relatime - xfs /dev/vdc rw,prjquota
42 22 0:6 / /dev rw,nosuid,relatime - devtmpfs devtmpfs rw
`

func TestRemountFlags_ACarriedMountKeepsItsNodevNosuidAndNoexecInTheReadOnlyRemount(t *testing.T) {
	entries, err := mountInfo(strings.NewReader(flagged))
	assert.NoError(t, err)
	const all = unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC
	got := remountFlags(entries, "/var/lib/naru/storage/locked/naru-volumes/local-a", true, true)
	assert.EqualValues(t, all|unix.MS_RDONLY, got, "the source's flags and read-only")
	// The control: a source mount with none of the flags still gets nodev and nosuid for a directory.
	got = remountFlags(entries, "/var/lib/naru/storage/plain/naru-volumes/local-a", true, false)
	assert.EqualValues(t, unix.MS_NODEV|unix.MS_NOSUID, got)
	got = remountFlags(entries, "/var/lib/naru/storage/plain/naru-volumes/local-a", true, true)
	assert.EqualValues(t, unix.MS_NODEV|unix.MS_NOSUID|unix.MS_RDONLY, got)
}

func TestRemountFlags_ADeviceNodeKeepsOnlyWhatItsMountHasSoThePodCanUseIt(t *testing.T) {
	entries, _ := mountInfo(strings.NewReader(flagged))
	assert.EqualValues(t, unix.MS_NOSUID, remountFlags(entries, "/dev/vdb", false, false))
	assert.EqualValues(t, unix.MS_NOSUID|unix.MS_RDONLY, remountFlags(entries, "/dev/vdb", false, true))
}
