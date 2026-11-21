package volumes

import (
	"strings"
	"testing"
	"unsafe"
)

const sample = `22 1 8:1 / / rw - ext4 /dev/root rw
40 22 253:16 / /var/lib/naru/storage/data rw,relatime - xfs /dev/vdb rw,prjquota
41 22 253:32 / /var/lib/naru/storage/plain rw - xfs /dev/vdc rw
42 22 253:48 / /mnt/with\040space rw - ext4 /dev/vdd rw
43 22 253:64 / /mnt/ext rw - ext4 /dev/vde rw,prjquota
`

func TestProjectQuotaOf(t *testing.T) {
	entries, err := mountInfo(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	for dir, ok := range map[string]bool{
		"/var/lib/naru/storage/data/naru-volumes/x": true,
		"/var/lib/naru/storage/plain/naru-volumes":  false,
		"/var/lib/naru":     false,
		"/mnt/ext/x":        false,
		"/mnt/with space/x": false,
	} {
		if got := projectQuotaOf(entries, dir) == nil; got != ok {
			t.Errorf("projectQuotaOf(%s) ok = %v, want %v", dir, got, ok)
		}
	}
	if m, _ := holding(entries, "/var/lib/naru/storage/data/a"); m.source != "/dev/vdb" {
		t.Errorf("holding = %+v", m)
	}
	if !isMountPoint(entries, "/mnt/with space") {
		t.Error("an escaped mount point is not read")
	}
	if _, err := mountInfo(strings.NewReader("garbage\n")); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestKernelStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(diskQuota{}); got != 112 {
		t.Errorf("fs_disk_quota is %d bytes, the kernel's is 112", got)
	}
	if got := unsafe.Sizeof(fsxattr{}); got != 28 {
		t.Errorf("fsxattr is %d bytes, the kernel's is 28", got)
	}
}
