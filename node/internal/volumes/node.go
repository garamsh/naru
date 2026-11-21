package volumes

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{Capabilities: []*csi.NodeServiceCapability{
		{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS}}},
	}}, nil
}

// NodeGetVolumeStats reports the bytes and inodes a local volume uses of its quota, or the size of a
// block volume's disk.
func (d *Driver) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
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
	if r.Kind == kindBlock {
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{{Unit: csi.VolumeUsage_BYTES, Total: r.Bytes}}}, nil
	}
	u, err := usage(d.dataDir(id))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: u}, nil
}

func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:             d.node,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKey: d.node}},
	}, nil
}

// NodePublishVolume binds a local volume's directory, or a block volume's device node, at the
// target path. A target that is already mounted is the answer to a retry.
func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is empty")
	}
	block, err := d.checkCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()})
	if err != nil {
		return nil, err
	}
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
	if block != (r.Kind == kindBlock) {
		return nil, status.Errorf(codes.InvalidArgument, "the volume %s is not of the requested mode", id)
	}
	source := d.dataDir(id)
	if !block {
		// The mount may have lost prjquota since the volume was made; a Pod must not write past its size.
		if err := d.quota.Enabled(d.cfg.StorageDir); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "the storage disk would not enforce the size: %v", err)
		}
	}
	if block {
		disk, err := d.disks.Find(r.Serial)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "the disk of the volume: %v", err)
		}
		source = disk.Path
		err = os.MkdirAll(filepath.Dir(target), 0o750)
		if err == nil {
			var f *os.File
			if f, err = os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
				err = f.Close()
			}
		}
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mounted, err := d.mnt.Mounted(target); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	} else if mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := d.mnt.Bind(source, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "bind %s at %s: %v", source, target, err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume undoes NodePublishVolume; a target that is gone is a success.
func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is empty")
	}
	mounted, err := d.mnt.Mounted(target)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mounted {
		if err := d.mnt.Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}
