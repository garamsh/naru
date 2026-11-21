package volumes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/naru/node/internal/volumes"
)

type fakeQuota struct {
	err   error
	limit map[string]int64
	proj  map[string]uint32
}

func (q *fakeQuota) Enabled(string) error { return q.err }
func (q *fakeQuota) Set(dir string, project uint32, bytes int64) error {
	q.limit[dir], q.proj[dir] = bytes, project
	return nil
}

type fakeMounts struct{ mounted map[string]string }

func (m *fakeMounts) Bind(source, target string, ro bool) error {
	m.mounted[target] = source
	return nil
}
func (m *fakeMounts) Unmount(target string) error { delete(m.mounted, target); return nil }
func (m *fakeMounts) Mounted(target string) (bool, error) {
	_, ok := m.mounted[target]
	return ok, nil
}

type fakeDisks map[string]volumes.Disk

func (f fakeDisks) Find(serial string) (volumes.Disk, error) {
	if d, ok := f[serial]; ok {
		return d, nil
	}
	return volumes.Disk{}, errors.New("absent")
}

type rig struct {
	d     *volumes.Driver
	q     *fakeQuota
	m     *fakeMounts
	state string
	store string
}

func newRig(t *testing.T, storage bool, serials ...string) *rig {
	t.Helper()
	r := &rig{q: &fakeQuota{limit: map[string]int64{}, proj: map[string]uint32{}}, m: &fakeMounts{mounted: map[string]string{}}, state: t.TempDir()}
	cfg := volumes.Config{StateDir: r.state, BlockSerials: serials}
	if storage {
		r.store = t.TempDir()
		cfg.StorageDir = r.store
	}
	disks := fakeDisks{"d1": {Path: "/dev/vdb", Bytes: 10 << 30}, "d2": {Path: "/dev/vdc", Bytes: 1 << 30}}
	r.d = volumes.New("n1", cfg, r.q, r.m, disks)
	return r
}

func mountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func create(r *rig, name string, bytes int64, c *csi.VolumeCapability) (*csi.CreateVolumeResponse, error) {
	return r.d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: name, VolumeCapabilities: []*csi.VolumeCapability{c},
		CapacityRange: &csi.CapacityRange{RequiredBytes: bytes},
	})
}

func code(err error) codes.Code { return status.Code(err) }

func TestCreateLocalSetsQuotaAndIsIdempotent(t *testing.T) {
	r := newRig(t, true)
	resp, err := create(r, "pvc-a", 5<<20, mountCap())
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Volume.VolumeId
	dir := filepath.Join(r.store, "naru-volumes", id)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("no directory %s: %v", dir, err)
	}
	if r.q.limit[dir] != 5<<20 || r.q.proj[dir] == 0 {
		t.Fatalf("quota of %s = %d project %d", dir, r.q.limit[dir], r.q.proj[dir])
	}
	again, err := create(r, "pvc-a", 5<<20, mountCap())
	if err != nil || again.Volume.VolumeId != id {
		t.Fatalf("second create = %v, %v; want the same volume %s", again, err, id)
	}
	if _, err := create(r, "pvc-a", 6<<20, mountCap()); code(err) != codes.AlreadyExists {
		t.Fatalf("create with another size: %v", err)
	}
	other, err := create(r, "pvc-b", 1<<20, mountCap())
	if err != nil {
		t.Fatal(err)
	}
	if r.q.proj[filepath.Join(r.store, "naru-volumes", other.Volume.VolumeId)] == r.q.proj[dir] {
		t.Fatal("two volumes share a project")
	}
}

func TestCreateLocalRefusals(t *testing.T) {
	r := newRig(t, false)
	if _, err := create(r, "pvc-a", 1<<20, mountCap()); code(err) != codes.FailedPrecondition {
		t.Fatalf("no storage disk: %v", err)
	}
	r = newRig(t, true)
	r.q.err = errors.New("no prjquota")
	if _, err := create(r, "pvc-a", 1<<20, mountCap()); code(err) != codes.FailedPrecondition {
		t.Fatalf("no project quota: %v", err)
	}
	entries, _ := os.ReadDir(r.state)
	if len(entries) != 0 {
		t.Fatalf("a refused create left %d records", len(entries))
	}
	r = newRig(t, true)
	if _, err := create(r, "Bad_Name", 1, mountCap()); code(err) != codes.InvalidArgument {
		t.Fatalf("bad name: %v", err)
	}
	multi := mountCap()
	multi.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	if _, err := create(r, "pvc-a", 1, multi); code(err) != codes.InvalidArgument {
		t.Fatalf("multi-node access: %v", err)
	}
	other := mountCap()
	other.GetMount().FsType = "ext4"
	if _, err := create(r, "pvc-a", 1, other); code(err) != codes.InvalidArgument {
		t.Fatalf("ext4: %v", err)
	}
}

func TestCreateRefusesAnotherNode(t *testing.T) {
	r := newRig(t, true)
	_, err := r.d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-a", VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		AccessibilityRequirements: &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{volumes.TopologyKey: "n2"}}}},
	})
	if code(err) != codes.ResourceExhausted {
		t.Fatalf("topology of another Node: %v", err)
	}
}

func TestDeleteLocalRemovesDirectoryAndLiftsQuota(t *testing.T) {
	r := newRig(t, true)
	resp, _ := create(r, "pvc-a", 1<<20, mountCap())
	id := resp.Volume.VolumeId
	dir := filepath.Join(r.store, "naru-volumes", id)
	if _, err := r.d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still there: %v", err)
	}
	if r.q.limit[dir] != 0 {
		t.Fatalf("quota not lifted: %d", r.q.limit[dir])
	}
	if _, err := r.d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestBlockClaimsDisksInOrderAndNeverWipes(t *testing.T) {
	r := newRig(t, false, "d2", "d1", "gone")
	// d2 is too small for 2 GiB and is skipped; d1 is claimed.
	a, err := create(r, "pvc-a", 2<<30, blockCap())
	if err != nil {
		t.Fatal(err)
	}
	if a.Volume.VolumeId != "block-d1" {
		t.Fatalf("claimed %s, want block-d1", a.Volume.VolumeId)
	}
	if again, err := create(r, "pvc-a", 2<<30, blockCap()); err != nil || again.Volume.VolumeId != "block-d1" {
		t.Fatalf("idempotent create: %v, %v", again, err)
	}
	if _, err := create(r, "pvc-b", 2<<30, blockCap()); code(err) != codes.ResourceExhausted {
		t.Fatalf("no disk left: %v", err)
	}
	if _, err := create(r, "pvc-c", 1<<30, blockCap()); err != nil {
		t.Fatalf("d2 fits 1 GiB: %v", err)
	}
	if _, err := r.d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "block-d1"}); err != nil {
		t.Fatal(err)
	}
	if len(r.q.limit) != 0 {
		t.Fatalf("deleting a block volume touched quotas: %v", r.q.limit)
	}
	if b, err := create(r, "pvc-b", 2<<30, blockCap()); err != nil || b.Volume.VolumeId != "block-d1" {
		t.Fatalf("a deleted claim frees its disk: %v, %v", b, err)
	}
}

func TestPublishBindsAndUnpublishUnbinds(t *testing.T) {
	r := newRig(t, true, "d1")
	resp, _ := create(r, "pvc-a", 1<<20, mountCap())
	id := resp.Volume.VolumeId
	target := filepath.Join(t.TempDir(), "pods", "p", "mount")
	pub := &csi.NodePublishVolumeRequest{VolumeId: id, TargetPath: target, VolumeCapability: mountCap()}
	for range 2 {
		if _, err := r.d.NodePublishVolume(context.Background(), pub); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(r.store, "naru-volumes", id)
	if r.m.mounted[target] != want {
		t.Fatalf("mounted %q, want %q", r.m.mounted[target], want)
	}
	if _, err := r.d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: id, TargetPath: target + "2", VolumeCapability: blockCap()}); code(err) != codes.InvalidArgument {
		t.Fatalf("block mode on a local volume: %v", err)
	}
	if _, err := r.d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "nope", TargetPath: target, VolumeCapability: mountCap()}); code(err) != codes.NotFound {
		t.Fatalf("unknown volume: %v", err)
	}
	for range 2 {
		if _, err := r.d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: id, TargetPath: target}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.m.mounted) != 0 {
		t.Fatalf("still mounted: %v", r.m.mounted)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target remains: %v", err)
	}
}

func TestPublishBlockBindsTheDeviceNode(t *testing.T) {
	r := newRig(t, false, "d1")
	resp, err := create(r, "pvc-a", 1<<30, blockCap())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dev")
	_, err = r.d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: resp.Volume.VolumeId, TargetPath: target, VolumeCapability: blockCap()})
	if err != nil {
		t.Fatal(err)
	}
	if r.m.mounted[target] != "/dev/vdb" {
		t.Fatalf("bound %q, want /dev/vdb", r.m.mounted[target])
	}
	if st, err := os.Stat(target); err != nil || st.IsDir() {
		t.Fatalf("a block target is a file: %v", err)
	}
}

func TestIdentityAndNodeInfo(t *testing.T) {
	r := newRig(t, true)
	info, err := r.d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil || info.Name != "naru-volumes" {
		t.Fatalf("plugin info %v, %v", info, err)
	}
	n, err := r.d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil || n.NodeId != "n1" || n.AccessibleTopology.Segments[volumes.TopologyKey] != "n1" {
		t.Fatalf("node info %v, %v", n, err)
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"stateDir":"/s","storageDir":"/d","blockSerials":["a"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := volumes.LoadConfig(p)
	if err != nil || c.StateDir != "/s" || c.StorageDir != "/d" || len(c.BlockSerials) != 1 {
		t.Fatalf("%+v, %v", c, err)
	}
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.LoadConfig(p); err == nil {
		t.Fatal("an empty stateDir was accepted")
	}
}
