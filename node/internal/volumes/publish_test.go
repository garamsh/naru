package volumes_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestNodePublishVolume_BindsTheDirectoryOnceAndUnpublishUnbindsIt(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	target := filepath.Join(t.TempDir(), "pods", "p", "mount")
	for range 2 {
		require.NoError(t, publish(r, id, target, mountCap()))
	}
	assert.Equal(t, r.dir(id), r.m.mounted[target])
	for range 2 {
		_, err := r.d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: id, TargetPath: target})
		require.NoError(t, err)
	}
	assert.Empty(t, r.m.mounted)
	assert.NoDirExists(t, target)
}

func TestNodePublishVolume_RefusesTheWrongModeAndAnUnknownVolumeAndAcceptsTheRightOne(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	target := filepath.Join(t.TempDir(), "m")
	assert.Equal(t, codes.InvalidArgument, code(publish(r, id, target, blockCap())))
	assert.Equal(t, codes.NotFound, code(publish(r, "local-nope", target, mountCap())))
	assert.NoError(t, publish(r, id, target, mountCap()))
}

func TestNodePublishVolume_RefusesALocalVolumeWhoseDiskLostItsQuota(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	target := filepath.Join(t.TempDir(), "m")
	r.q.err = assert.AnError
	assert.Equal(t, codes.FailedPrecondition, code(publish(r, id, target, mountCap())))
	assert.Empty(t, r.m.mounted)
	r.q.err = nil
	assert.NoError(t, publish(r, id, target, mountCap()))
}

func TestNodePublishVolume_BindsABlockVolumesDeviceNodeAsAFile(t *testing.T) {
	r := newRig(t, false, "d1")
	id := mustCreate(t, r, "pvc-a", 1<<30, blockCap())
	target := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, publish(r, id, target, blockCap()))
	assert.Equal(t, "/dev/vdb", r.m.mounted[target])
	assert.FileExists(t, target)
}

func TestVolumeID_APathInAnIDReachesNoFileOutsideTheVolumes(t *testing.T) {
	r := newRig(t, true)
	mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	target := filepath.Join(t.TempDir(), "m")
	for _, bad := range []string{"local-../../etc", "local-a/b", "block-../x", "../local-a", "local-", "local-.hidden", "other-a", "", "local-a\x00"} {
		assert.Equal(t, codes.InvalidArgument, code(publish(r, bad, target, mountCap())), "publish %q", bad)
		assert.Equal(t, codes.InvalidArgument, code(remove(r, bad)), "delete %q", bad)
	}
	assert.Empty(t, r.m.mounted)
	assert.NoError(t, publish(r, "local-pvc-a", target, mountCap()), "the control: a well-formed id is served")
}

func TestNodePublishVolume_AFailedHardeningLeavesNoMountForARetryToTake(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	target := filepath.Join(t.TempDir(), "m")
	r.m.bindErr = assert.AnError
	assert.Equal(t, codes.Internal, code(publish(r, id, target, mountCap())))
	assert.Empty(t, r.m.mounted, "the bind is undone")
	r.m.bindErr = nil
	require.NoError(t, publish(r, id, target, mountCap()))
	assert.Equal(t, r.dir(id), r.m.mounted[target], "the retry binds, not skips")
}

func TestNodePublishVolume_RefusesABlockVolumeReadOnlyAndAcceptsALocalOne(t *testing.T) {
	r := newRig(t, false, "d1")
	id := mustCreate(t, r, "pvc-a", 1<<30, blockCap())
	target := filepath.Join(t.TempDir(), "dev")
	assert.Equal(t, codes.InvalidArgument, code(publishReadOnly(r, id, target, blockCap())))
	assert.Empty(t, r.m.mounted)
	assert.NoFileExists(t, target)
	require.NoError(t, publish(r, id, target, blockCap()), "the control: read-write is served")

	local := newRig(t, true)
	lid := mustCreate(t, local, "pvc-b", 1<<20, mountCap())
	assert.NoError(t, publishReadOnly(local, lid, filepath.Join(t.TempDir(), "m"), mountCap()), "the control: a directory may be read-only")
}
