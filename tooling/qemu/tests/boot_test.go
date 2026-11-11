//go:build qemu_real

// Package tests boots naru's image under QEMU. It needs the image built
// (make image), /dev/kvm and mke2fs; `make test-qemu` runs it.
package tests_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

const bootTimeout = 90 * time.Second

func imageDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("NARU_IMAGE_DIR")
	require.NotEmpty(t, dir, "NARU_IMAGE_DIR names the directory make image writes")
	return dir
}

func start(t *testing.T, naruYAML []byte) *node.Node {
	t.Helper()
	img := imageDir(t)
	n, err := node.Start(node.Spec{
		Name:     "node-1",
		Kernel:   filepath.Join(img, "bzImage"),
		Initrd:   filepath.Join(img, "initramfs.cpio.gz"),
		Dir:      t.TempDir(),
		NaruYAML: naruYAML,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		n.Stop()
		t.Logf("console:\n%s", n.Console())
	})
	return n
}

func TestNode_BootsAndStartsItsServices(t *testing.T) {
	n := start(t, nil)
	ctx := context.Background()

	require.NoError(t, n.WaitFor(ctx, bootTimeout, "configuration accepted", "node=naru5-node-1"), "init accepted the configuration")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "started", "service=mvmd"), "init started mvmd")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "started", "service=standin"), "init started the stand-in")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "data disk is mounted", "dir=/var/lib/mvmd", "fstype=ext4"), "the data disk is mounted")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "socket accepts connections", "path=/run/mvmd.sock"), "mvmd serves on its socket")
}
