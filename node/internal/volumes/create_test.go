package volumes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/garamsh/naru/node/internal/volumes"
)

func TestCreateVolume_LocalSetsTheQuotaAndAnotherProjectForTheNextVolume(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 5<<20, mountCap())
	st, err := os.Stat(r.dir(id))
	require.NoError(t, err)
	assert.True(t, st.IsDir())
	assert.EqualValues(t, 5<<20, r.q.limit[r.dir(id)])
	assert.NotZero(t, r.q.proj[r.dir(id)])
	other := mustCreate(t, r, "pvc-b", 1<<20, mountCap())
	assert.NotEqual(t, r.q.proj[r.dir(id)], r.q.proj[r.dir(other)], "two volumes share a project")
}

func TestCreateVolume_AnswersARetryWithTheSameVolumeAndRefusesAnotherSize(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 5<<20, mountCap())
	assert.Equal(t, id, mustCreate(t, r, "pvc-a", 5<<20, mountCap()))
	_, err := create(r, "pvc-a", 6<<20, mountCap())
	assert.Equal(t, codes.AlreadyExists, code(err))
	_, err = create(r, "pvc-a", 5<<20, blockCap())
	assert.Equal(t, codes.AlreadyExists, code(err), "the name is a local volume, not a block one")
}

func TestCreateVolume_RefusesWhatItCannotMakeAndAcceptsTheSameRequestWhenItCan(t *testing.T) {
	t.Run("no storage disk", func(t *testing.T) {
		_, err := create(newRig(t, false), "pvc-a", 1<<20, mountCap())
		assert.Equal(t, codes.FailedPrecondition, code(err))
		_, err = create(newRig(t, true), "pvc-a", 1<<20, mountCap())
		assert.NoError(t, err)
	})
	t.Run("storage disk without project quota", func(t *testing.T) {
		r := newRig(t, true)
		r.q.err = errors.New("no prjquota")
		_, err := create(r, "pvc-a", 1<<20, mountCap())
		assert.Equal(t, codes.FailedPrecondition, code(err))
		entries, _ := os.ReadDir(r.cfg.StateDir)
		assert.Empty(t, entries, "a refused create leaves no record")
		r.q.err = nil
		_, err = create(r, "pvc-a", 1<<20, mountCap())
		assert.NoError(t, err)
	})
	t.Run("claim larger than the room", func(t *testing.T) {
		r := newRig(t, true)
		r.q.free = 4 << 20
		_, err := create(r, "pvc-a", 5<<20, mountCap())
		assert.Equal(t, codes.ResourceExhausted, code(err))
		_, err = create(r, "pvc-a", 4<<20, mountCap())
		assert.NoError(t, err, "a claim that fits is made")
	})
	t.Run("bad name", func(t *testing.T) {
		r := newRig(t, true)
		_, err := create(r, "Bad_Name", 1, mountCap())
		assert.Equal(t, codes.InvalidArgument, code(err))
		_, err = create(r, "good-name", 1, mountCap())
		assert.NoError(t, err)
	})
	t.Run("access mode spanning Nodes", func(t *testing.T) {
		r := newRig(t, true)
		multi := mountCap()
		multi.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		_, err := create(r, "pvc-a", 1, multi)
		assert.Equal(t, codes.InvalidArgument, code(err))
		_, err = create(r, "pvc-a", 1, mountCap())
		assert.NoError(t, err)
	})
	t.Run("file system other than xfs", func(t *testing.T) {
		r := newRig(t, true)
		ext4 := mountCap()
		ext4.GetMount().FsType = "ext4"
		_, err := create(r, "pvc-a", 1, ext4)
		assert.Equal(t, codes.InvalidArgument, code(err))
		xfs := mountCap()
		xfs.GetMount().FsType = "xfs"
		_, err = create(r, "pvc-a", 1, xfs)
		assert.NoError(t, err)
	})
}

func TestCreateVolume_RefusesAContentSourceAndStorageClassParameters(t *testing.T) {
	r := newRig(t, true)
	req := &csi.CreateVolumeRequest{Name: "pvc-a", VolumeCapabilities: []*csi.VolumeCapability{mountCap()}}
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "s"}}}
	_, err := r.d.CreateVolume(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, code(err), "a restore would be an empty volume")
	req.VolumeContentSource = nil
	req.Parameters = map[string]string{"size": "fast"}
	_, err = r.d.CreateVolume(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, code(err), "an unknown parameter is a typo")
	req.Parameters = nil
	_, err = r.d.CreateVolume(context.Background(), req)
	assert.NoError(t, err)
}

func TestCreateVolume_RefusesAnotherNode(t *testing.T) {
	r := newRig(t, true)
	req := &csi.CreateVolumeRequest{Name: "pvc-a", VolumeCapabilities: []*csi.VolumeCapability{mountCap()}}
	req.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{volumes.TopologyKey: "n2"}}}}
	_, err := r.d.CreateVolume(context.Background(), req)
	assert.Equal(t, codes.ResourceExhausted, code(err))
	req.AccessibilityRequirements.Requisite[0].Segments[volumes.TopologyKey] = "n1"
	_, err = r.d.CreateVolume(context.Background(), req)
	assert.NoError(t, err)
}

func TestCreateVolume_FailedQuotaLeavesNoRecordAndNoDirectory(t *testing.T) {
	r := newRig(t, true)
	r.q.setErr = errors.New("quotactl")
	_, err := create(r, "pvc-a", 1<<20, mountCap())
	assert.Equal(t, codes.Internal, code(err))
	entries, _ := os.ReadDir(r.cfg.StateDir)
	assert.Empty(t, entries, "the record is written after the volume is made")
	assert.NoDirExists(t, r.dir("local-pvc-a"))
}

func TestCreateVolume_AfterACrashBetweenTheDirectoryAndTheRecordMakesTheVolumeAgain(t *testing.T) {
	r := newRig(t, true)
	// The process died after the directory and its quota and before the record.
	stale := filepath.Join(r.dir("local-pvc-a"), "left-over")
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o755))
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	assert.NoFileExists(t, stale)
	assert.DirExists(t, r.dir(id))
	assert.EqualValues(t, 1<<20, r.q.limit[r.dir(id)])
}

func TestCreateVolume_SurvivesARestartWithItsRecordsAndGivesNoProjectTwice(t *testing.T) {
	r := newRig(t, true, "d1")
	a := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	disk := mustCreate(t, r, "pvc-disk", 1<<30, blockCap())
	r.restart()
	assert.Equal(t, a, mustCreate(t, r, "pvc-a", 1<<20, mountCap()), "the record outlives the process")
	assert.Equal(t, disk, mustCreate(t, r, "pvc-disk", 1<<30, blockCap()))
	_, err := create(r, "pvc-other", 1<<30, blockCap())
	assert.Equal(t, codes.ResourceExhausted, code(err), "the disk stays claimed after a restart")
	b := mustCreate(t, r, "pvc-b", 1<<20, mountCap())
	assert.NotEqual(t, r.q.proj[r.dir(a)], r.q.proj[r.dir(b)])
}

func TestCreateVolume_AReleasedProjectIsGivenAgainOnlyAfterItsLimitIsLifted(t *testing.T) {
	r := newRig(t, true)
	a := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	b := mustCreate(t, r, "pvc-b", 1<<20, mountCap())
	project := r.q.proj[r.dir(b)]
	require.NoError(t, remove(r, b))
	assert.Zero(t, r.q.limit[r.dir(b)], "the deleted volume's project is unlimited")
	c := mustCreate(t, r, "pvc-c", 2<<20, mountCap())
	assert.Equal(t, project, r.q.proj[r.dir(c)], "the highest project is given again")
	assert.EqualValues(t, 2<<20, r.q.limit[r.dir(c)])
	assert.NotEqual(t, r.q.proj[r.dir(a)], r.q.proj[r.dir(c)])
}

func TestCreateVolume_ACorruptRecordBlocksOnlyVolumesOfItsKind(t *testing.T) {
	r := newRig(t, true, "d1")
	require.NoError(t, os.WriteFile(filepath.Join(r.cfg.StateDir, "local-broken.json"), []byte("{"), 0o600))
	_, err := create(r, "pvc-a", 1<<20, mountCap())
	assert.Equal(t, codes.Internal, code(err), "its project is unknown")
	assert.Contains(t, err.Error(), "local-broken")
	_, err = create(r, "pvc-disk", 1<<30, blockCap())
	assert.NoError(t, err, "a block volume does not depend on a local record")
}
