package volumes

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
)

// usage reports the bytes and inodes of the file system holding dir. On a directory with a project
// quota XFS answers with the project's limit and use, which is the volume's own.
func usage(dir string) ([]*csi.VolumeUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil, err
	}
	bs := int64(st.Bsize)
	return []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: int64(st.Blocks) * bs, Available: int64(st.Bavail) * bs, Used: int64(st.Blocks-st.Bfree) * bs},
		{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files - st.Ffree)},
	}, nil
}
