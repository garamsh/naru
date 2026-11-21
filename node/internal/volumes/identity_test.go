package volumes_test

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/node/internal/volumes"
)

func TestGetPluginInfoAndNodeGetInfo_NameTheDriverAndTheNode(t *testing.T) {
	r := newRig(t, true)
	info, err := r.d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "naru-volumes", info.Name)
	n, err := r.d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "n1", n.NodeId)
	assert.Equal(t, "n1", n.AccessibleTopology.Segments[volumes.TopologyKey])
}
