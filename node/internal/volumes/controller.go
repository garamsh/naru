package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultBytes is the size of a local volume whose claim gives none.
const defaultBytes = 1 << 30

func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{
		{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME}}},
	}}, nil
}

// CreateVolume makes a local volume, or hands out an unclaimed disk when the claim asks for a block
// volume. It is idempotent on the request's name.
func (d *Driver) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	block, err := d.checkCapabilities(req.GetVolumeCapabilities())
	if err != nil {
		return nil, err
	}
	kind := kindLocal
	if block {
		kind = kindBlock
	}
	id, err := newID(kind, req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument, "a volume is made empty: the driver clones and restores nothing")
	}
	if len(req.GetParameters()) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "the driver takes no StorageClass parameters, and the class gives %v", req.GetParameters())
	}
	if !d.accessible(req.GetAccessibilityRequirements()) {
		return nil, status.Errorf(codes.ResourceExhausted, "the volume is to be on a Node other than %s", d.node)
	}
	bytes := req.GetCapacityRange().GetRequiredBytes()
	if bytes == 0 {
		bytes = req.GetCapacityRange().GetLimitBytes()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if resp, err := d.existing(id, block, bytes); resp != nil || err != nil {
		return resp, err
	}
	all, corrupt, err := d.records()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// A record that cannot be read hides a project or a disk it holds, so a volume of its kind is not
	// made while it stays; every other call is served.
	for _, c := range corrupt {
		if c.kind() == kind {
			return nil, status.Errorf(codes.Internal, "the record of %s cannot be read: remove it to make volumes again", c)
		}
	}
	if block {
		return d.createBlock(id, req.GetName(), bytes, all)
	}
	return d.createLocal(id, req.GetName(), bytesOrDefault(bytes), all)
}

// existing answers the retry of a create: the volume that carries the id, or a refusal when it is
// another one. Neither is returned when nothing has the name.
func (d *Driver) existing(id volumeID, block bool, bytes int64) (*csi.CreateVolumeResponse, error) {
	for _, other := range []string{kindLocal, kindBlock} {
		oid := volumeID(other + string(id)[len(id.kind()):])
		r, err := d.load(oid)
		if errors.Is(err, errNoRecord) {
			continue
		}
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if oid != id || (!block && r.Bytes != bytesOrDefault(bytes)) {
			return nil, status.Errorf(codes.AlreadyExists, "the volume %s exists with another kind or size", r.Name)
		}
		return d.created(oid, r), nil
	}
	return nil, nil
}

func bytesOrDefault(b int64) int64 {
	if b == 0 {
		return defaultBytes
	}
	return b
}

func (d *Driver) created(id volumeID, r record) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: string(id), CapacityBytes: r.Bytes,
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{TopologyKey: d.node}}},
	}}
}

// createLocal makes the directory and its quota first and writes the record last, so a record always
// names a volume that exists. A directory with no record is what a crash left, and nobody uses it:
// it is removed and made again.
func (d *Driver) createLocal(id volumeID, name string, bytes int64, all map[volumeID]record) (*csi.CreateVolumeResponse, error) {
	if d.cfg.StorageDir == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "Node %s has no storage disk", d.node)
	}
	if err := d.quota.Enabled(d.cfg.StorageDir); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the storage disk would not enforce the size: %v", err)
	}
	free, err := d.quota.Free(d.cfg.StorageDir)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if bytes > free {
		return nil, status.Errorf(codes.ResourceExhausted, "Node %s has %d bytes free, and the claim asks for %d", d.node, free, bytes)
	}
	r := record{Kind: kindLocal, Name: name, Bytes: bytes, Project: nextProject(all)}
	dir := d.dataDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := d.makeLocal(dir, r); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("make the volume: %v", errors.Join(err, os.RemoveAll(dir))))
	}
	if err := d.save(id, r); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("record the volume: %v", errors.Join(err, os.RemoveAll(dir))))
	}
	return d.created(id, r), nil
}

// makeLocal makes the directory of a local volume and limits it to the record's size. A Pod runs as
// any user, so the mode is open: the quota, not the mode, bounds what it writes.
func (d *Driver) makeLocal(dir string, r record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return err
	}
	return d.quota.Set(dir, r.Project, r.Bytes)
}

// createBlock hands out the first listed disk no volume holds that is at least bytes large and that
// nothing on the Node uses: a disk the Node mounted or maps over is its own, not the claim's.
func (d *Driver) createBlock(id volumeID, name string, bytes int64, all map[volumeID]record) (*csi.CreateVolumeResponse, error) {
	claimed := map[string]bool{}
	for _, r := range all {
		claimed[r.Serial] = true
	}
	var skipped []string
	for _, serial := range d.cfg.BlockSerials {
		if claimed[serial] {
			continue
		}
		disk, err := d.disks.Find(serial)
		if err == nil && disk.Bytes < bytes {
			err = fmt.Errorf("%d bytes are fewer than %d", disk.Bytes, bytes)
		}
		if err == nil {
			err = d.disks.InUse(disk)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", serial, err))
			continue
		}
		r := record{Kind: kindBlock, Name: name, Bytes: disk.Bytes, Serial: serial}
		if err := d.save(id, r); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return d.created(id, r), nil
	}
	return nil, status.Errorf(codes.ResourceExhausted, "Node %s has no unclaimed disk of %d bytes to offer (%s)", d.node, bytes, strings.Join(skipped, "; "))
}

// DeleteVolume removes a local volume's directory. A block volume's claim is forgotten and the
// disk is left as the Pod wrote it: the driver never wipes a disk.
func (d *Driver) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id, err := parseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	switch {
	case errors.Is(err, errNoRecord):
		return &csi.DeleteVolumeResponse{}, nil
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	if r.Kind == kindLocal {
		dir := d.dataDir(id)
		if err := d.quota.Set(dir, r.Project, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if err := d.forget(id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	id, err := parseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	r, err := d.load(id)
	if errors.Is(err, errNoRecord) {
		return nil, status.Errorf(codes.NotFound, "no volume %s", id)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	block, err := d.checkCapabilities(req.GetVolumeCapabilities())
	if err != nil || block != (r.Kind == kindBlock) {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: "the volume is not of the requested mode"}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: req.GetVolumeCapabilities()}}, nil
}

// checkCapabilities refuses a capability the driver cannot give and reports whether the volume is a
// block volume. A volume is on one Node, so only single-node access modes are given.
func (d *Driver) checkCapabilities(caps []*csi.VolumeCapability) (block bool, err error) {
	if len(caps) == 0 {
		return false, status.Error(codes.InvalidArgument, "the volume capabilities are empty")
	}
	for i, c := range caps {
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		default:
			return false, status.Errorf(codes.InvalidArgument, "the access mode %s spans Nodes, and a volume is on one", c.GetAccessMode().GetMode())
		}
		isBlock := c.GetBlock() != nil
		if i > 0 && isBlock != block {
			return false, status.Error(codes.InvalidArgument, "the capabilities mix block and file system modes")
		}
		if fsType := c.GetMount().GetFsType(); fsType != "" && fsType != "xfs" {
			return false, status.Errorf(codes.InvalidArgument, "the file system %q is not xfs, which a local volume is", fsType)
		}
		block = isBlock
	}
	return block, nil
}

// accessible reports whether the Node is among the ones the request allows.
func (d *Driver) accessible(req *csi.TopologyRequirement) bool {
	if req == nil {
		return true
	}
	for _, list := range [][]*csi.Topology{req.GetPreferred(), req.GetRequisite()} {
		for _, t := range list {
			if t.GetSegments()[TopologyKey] == d.node {
				return true
			}
		}
	}
	return len(req.GetPreferred()) == 0 && len(req.GetRequisite()) == 0
}
