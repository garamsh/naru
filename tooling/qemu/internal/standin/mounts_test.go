package standin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleMountinfo = `22 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
36 1 254:16 / /var/lib/mvmd rw,nosuid,nodev,relatime shared:2 - ext4 /dev/vdb rw
37 1 254:0 / /run/naru/config ro,noexec,relatime shared:3 - ext4 /dev/vda ro
`

func TestMountOf_ReportsTypeSourceAndOptionsOfTheMountPoint(t *testing.T) {
	m, err := mountOf(strings.NewReader(sampleMountinfo), "/var/lib/mvmd")

	require.NoError(t, err)
	assert.Equal(t, Mount{Dir: "/var/lib/mvmd", FSType: "ext4", Source: "/dev/vdb", Options: "rw,nosuid,nodev,relatime,rw"}, m)
}

func TestMountOf_FailsWhenDirIsNotAMountPoint(t *testing.T) {
	_, err := mountOf(strings.NewReader(sampleMountinfo), "/var/lib")

	assert.ErrorContains(t, err, "/var/lib is not a mount point")
}

func TestMountOf_TakesTheLastMountAtAMountPoint(t *testing.T) {
	info := sampleMountinfo + "40 1 7:0 / /var/lib/mvmd rw - tmpfs tmpfs rw\n"

	m, err := mountOf(strings.NewReader(info), "/var/lib/mvmd")

	require.NoError(t, err)
	assert.Equal(t, "tmpfs", m.FSType)
}
