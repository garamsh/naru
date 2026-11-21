package volumes_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/naru/node/internal/volumes"
)

type fakeQuota struct {
	err, setErr error
	free        int64
	limit       map[string]int64
	proj        map[string]uint32
}

func (q *fakeQuota) Enabled(string) error       { return q.err }
func (q *fakeQuota) Free(string) (int64, error) { return q.free, nil }
func (q *fakeQuota) Set(dir string, project uint32, bytes int64) error {
	if q.setErr != nil {
		return q.setErr
	}
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

type fakeDisks struct {
	disks map[string]volumes.Disk
	// busy maps a device node to what uses it.
	busy map[string]error
}

func (f fakeDisks) Find(serial string) (volumes.Disk, error) {
	if d, ok := f.disks[serial]; ok {
		return d, nil
	}
	return volumes.Disk{}, errors.New("absent")
}
func (f fakeDisks) InUse(d volumes.Disk) error { return f.busy[d.Path] }

type rig struct {
	d     *volumes.Driver
	q     *fakeQuota
	m     *fakeMounts
	disks fakeDisks
	cfg   volumes.Config
	store string
}

// newRig makes a driver of Node n1 with disks d1 (10 GiB, /dev/vdb) and d2 (1 GiB, /dev/vdc) and a
// storage disk when storage is set; serials are the ones it offers whole.
func newRig(t *testing.T, storage bool, serials ...string) *rig {
	t.Helper()
	r := &rig{
		q:   &fakeQuota{free: 100 << 30, limit: map[string]int64{}, proj: map[string]uint32{}},
		m:   &fakeMounts{mounted: map[string]string{}},
		cfg: volumes.Config{StateDir: t.TempDir(), BlockSerials: serials},
		disks: fakeDisks{
			disks: map[string]volumes.Disk{"d1": {Path: "/dev/vdb", Bytes: 10 << 30}, "d2": {Path: "/dev/vdc", Bytes: 1 << 30}},
			busy:  map[string]error{},
		},
	}
	if storage {
		r.store = t.TempDir()
		r.cfg.StorageDir = r.store
	}
	r.d = volumes.New("n1", r.cfg, r.q, r.m, r.disks)
	return r
}

// restart is the driver after the process was killed and started again on the same directories.
func (r *rig) restart() { r.d = volumes.New("n1", r.cfg, r.q, r.m, r.disks) }

func (r *rig) dir(id string) string { return filepath.Join(r.store, "naru-volumes", id) }

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

func mustCreate(t *testing.T, r *rig, name string, bytes int64, c *csi.VolumeCapability) string {
	t.Helper()
	resp, err := create(r, name, bytes, c)
	require.NoError(t, err)
	return resp.Volume.VolumeId
}

func remove(r *rig, id string) error {
	_, err := r.d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	return err
}

func publish(r *rig, id, target string, c *csi.VolumeCapability) error {
	_, err := r.d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: id, TargetPath: target, VolumeCapability: c})
	return err
}

func code(err error) codes.Code { return status.Code(err) }

// loadSerial is the disk a block volume's record names.
func (r *rig) loadSerial(id string) (string, error) {
	b, err := os.ReadFile(filepath.Join(r.cfg.StateDir, id+".json"))
	if err != nil {
		return "", err
	}
	var rec struct{ Serial string }
	return rec.Serial, json.Unmarshal(b, &rec)
}
