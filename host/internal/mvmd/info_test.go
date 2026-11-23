package mvmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// infoMvmd answers GetRuntimeInfo with a kernel and two boot sets, one configured and one added.
type infoMvmd struct {
	v1alpha2.UnimplementedRuntimeServiceServer
}

func (infoMvmd) GetRuntimeInfo(context.Context, *v1alpha2.GetRuntimeInfoRequest) (*v1alpha2.GetRuntimeInfoResponse, error) {
	return &v1alpha2.GetRuntimeInfoResponse{
		Kernels: []*v1alpha2.NodeKernel{{Name: "naru", Digest: "kd", IsDefault: true}},
		BootSets: []*v1alpha2.BootSet{
			{Name: "naru-node-a", KernelDigest: "k1", InitrdDigest: "i1", Origin: v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_CONFIGURED},
			{Name: "naru-node-b", KernelDigest: "k2", InitrdDigest: "i2", Origin: v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED},
		},
	}, nil
}

func TestInfo_ReportsTheBootSetsTheNodeOffersApartFromItsKernels(t *testing.T) {
	// A unix socket's path is at most 107 bytes, which t.TempDir() under a long test name exceeds.
	dir, err := os.MkdirTemp("", "mvmd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	v1alpha2.RegisterRuntimeServiceServer(srv, infoMvmd{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	g := New(sock)
	t.Cleanup(func() { _ = g.Close() })

	info, err := g.Info(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []Kernel{{Name: "naru", Digest: "kd", Default: true}}, info.Kernels)
	assert.Equal(t, []BootSet{
		{Name: "naru-node-a", KernelDigest: "k1", InitrdDigest: "i1", Origin: "configured"},
		{Name: "naru-node-b", KernelDigest: "k2", InitrdDigest: "i2", Origin: "added"},
	}, info.BootSets, "a boot set added through AddBootSet is offered, and it is not a kernel")
}
