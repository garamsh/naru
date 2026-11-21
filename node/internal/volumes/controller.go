package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultBytes is the size of a local volume whose claim gives none.
const defaultBytes = 1 << 30

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,200}$`)

func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{
		{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME}}},
	}}, nil
}

// CreateVolume makes a local volume, or hands out an unclaimed disk when the claim asks for a block
// volume. It is idempotent on the request's name.
func (d *Driver) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if !nameRE.MatchString(req.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "the volume name %q is not a lowercase DNS name", req.GetName())
	}
	block, err := d.checkCapabilities(req.GetVolumeCapabilities())
	if err != nil {
		return nil, err
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
	all, err := d.records()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	kind := kindLocal
	if block {
		kind = kindBlock
	}
	for id, r := range all {
		if r.Name == req.GetName() {
			if r.Kind != kind || (!block && r.Bytes != bytesOrDefault(bytes)) {
				return nil, status.Errorf(codes.AlreadyExists, "the volume %s exists with another kind or size", req.GetName())
			}
			return d.created(id, r), nil
		}
	}
	if block {
		return d.createBlock(req.GetName(), bytes, all)
	}
	return d.createLocal(req.GetName(), bytesOrDefault(bytes), all)
}

func bytesOrDefault(b int64) int64 {
	if b == 0 {
		return defaultBytes
	}
	return b
}

func (d *Driver) created(id string, r record) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: id, CapacityBytes: r.Bytes,
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{TopologyKey: d.node}}},
	}}
}

func (d *Driver) createLocal(name string, bytes int64, all map[string]record) (*csi.CreateVolumeResponse, error) {
	if d.cfg.StorageDir == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "Node %s has no storage disk", d.node)
	}
	if err := d.quota.Enabled(d.cfg.StorageDir); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the storage disk would not enforce the size: %v", err)
	}
	id := localPrefix + name
	r := record{Kind: kindLocal, Name: name, Bytes: bytes, Project: nextProject(all)}
	if err := d.save(id, r); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	dir := d.dataDir(id)
	if err := d.makeLocal(dir, r); err != nil {
		return nil, d.undo(id, dir, err)
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

// undo removes what a failed local volume made and returns the failure as a status.
func (d *Driver) undo(id, dir string, cause error) error {
	err := errors.Join(os.RemoveAll(dir), d.forget(id))
	return status.Error(codes.Internal, fmt.Sprintf("make the volume: %v", errors.Join(cause, err)))
}

// createBlock hands out the first listed disk no volume holds that is at least bytes large.
func (d *Driver) createBlock(name string, bytes int64, all map[string]record) (*csi.CreateVolumeResponse, error) {
	claimed := map[string]bool{}
	for _, r := range all {
		claimed[r.Serial] = true
	}
	for _, serial := range d.cfg.BlockSerials {
		if claimed[serial] {
			continue
		}
		disk, err := d.disks.Find(serial)
		if err != nil || disk.Bytes < bytes {
			continue
		}
		id := blockPrefix + serial
		r := record{Kind: kindBlock, Name: name, Bytes: disk.Bytes, Serial: serial}
		if err := d.save(id, r); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return d.created(id, r), nil
	}
	return nil, status.Errorf(codes.ResourceExhausted, "Node %s has no unclaimed disk of %d bytes to offer", d.node, bytes)
}

// DeleteVolume removes a local volume's directory. A block volume's claim is forgotten and the
// disk is left as the Pod wrote it: the driver never wipes a disk.
func (d *Driver) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
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
	r, err := d.load(req.GetVolumeId())
	if errors.Is(err, errNoRecord) {
		return nil, status.Errorf(codes.NotFound, "no volume %s", req.GetVolumeId())
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
