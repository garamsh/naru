package volumes_test

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestNodeGetVolumeStats_ReportsBytesAndInodesOfALocalVolumeAndTheSizeOfADisk(t *testing.T) {
	r := newRig(t, true, "d1")
	local := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	resp, err := r.d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: local, VolumePath: "/x"})
	require.NoError(t, err)
	require.Len(t, resp.Usage, 2)
	assert.Equal(t, csi.VolumeUsage_BYTES, resp.Usage[0].Unit)
	assert.Positive(t, resp.Usage[0].Total)
	block := mustCreate(t, r, "pvc-disk", 1<<30, blockCap())
	resp, err = r.d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: block, VolumePath: "/x"})
	require.NoError(t, err)
	assert.EqualValues(t, 10<<30, resp.Usage[0].Total)
	_, err = r.d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "local-nope", VolumePath: "/x"})
	assert.Equal(t, codes.NotFound, code(err))
}

func TestNodeGetCapabilities_OffersVolumeStats(t *testing.T) {
	r := newRig(t, true)
	resp, err := r.d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Capabilities, 1)
	assert.Equal(t, csi.NodeServiceCapability_RPC_GET_VOLUME_STATS, resp.Capabilities[0].GetRpc().Type)
}
