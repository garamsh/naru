package mvmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// bootSetServer is an mvmd that records the stream of an AddBootSet as it arrives.
type bootSetServer struct {
	v1alpha2.UnimplementedRuntimeServiceServer
	start        *v1alpha2.AddBootSetStart
	kernel, init bytes.Buffer
	// order is "k" or "i" for each chunk, in the order they arrived.
	order  string
	refuse error
	// endAtStart ends the stream with this error at the first message, before the files.
	endAtStart error
	removed    []string
}

func (s *bootSetServer) AddBootSet(stream grpc.ClientStreamingServer[v1alpha2.AddBootSetRequest, v1alpha2.AddBootSetResponse]) error {
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if s.refuse != nil {
				return s.refuse
			}
			return stream.SendAndClose(&v1alpha2.AddBootSetResponse{})
		}
		if err != nil {
			return err
		}
		switch p := m.GetPart().(type) {
		case *v1alpha2.AddBootSetRequest_Start:
			if s.endAtStart != nil {
				return s.endAtStart
			}
			s.start = p.Start
		case *v1alpha2.AddBootSetRequest_KernelChunk:
			s.kernel.Write(p.KernelChunk)
			s.order += "k"
		case *v1alpha2.AddBootSetRequest_InitrdChunk:
			s.init.Write(p.InitrdChunk)
			s.order += "i"
		}
	}
}

func (s *bootSetServer) RemoveBootSet(_ context.Context, r *v1alpha2.RemoveBootSetRequest) (*v1alpha2.RemoveBootSetResponse, error) {
	s.removed = append(s.removed, r.GetName())
	return &v1alpha2.RemoveBootSetResponse{}, s.refuse
}

func serve(t *testing.T, s *bootSetServer) *GRPC {
	t.Helper()
	// A unix socket's path is at most 107 bytes, which t.TempDir() under a long test name exceeds.
	dir, err := os.MkdirTemp("", "mvmd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	v1alpha2.RegisterRuntimeServiceServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	g := New(sock)
	t.Cleanup(func() { _ = g.Close() })
	_, err = os.Stat(sock)
	require.NoError(t, err)
	return g
}

func TestAddBootSet_SendsTheStartThenTheKernelThenTheInitrdInChunks(t *testing.T) {
	s := &bootSetServer{}
	g := serve(t, s)
	kernel := bytes.Repeat([]byte("k"), bootSetChunk+10)
	initrd := bytes.Repeat([]byte("i"), 3)

	err := g.AddBootSet(context.Background(), BootSet{Name: "naru-node-v1", KernelDigest: "kd", InitrdDigest: "id"}, bytes.NewReader(kernel), bytes.NewReader(initrd))
	require.NoError(t, err)
	assert.Equal(t, "naru-node-v1", s.start.GetName())
	assert.Equal(t, "kd", s.start.GetKernelDigest())
	assert.Equal(t, "id", s.start.GetInitrdDigest())
	assert.Equal(t, kernel, s.kernel.Bytes())
	assert.Equal(t, initrd, s.init.Bytes())
	assert.Equal(t, "kki", s.order, "two kernel chunks, then the initrd's")
}

func TestAddBootSet_MapsTheStatusOfARefusalAndAReadFailureIsNotAnOutcomeOfMvmd(t *testing.T) {
	s := &bootSetServer{refuse: status.Error(codes.InvalidArgument, "the kernel's SHA-256 is not kd")}
	g := serve(t, s)
	err := g.AddBootSet(context.Background(), BootSet{Name: "x"}, strings.NewReader("k"), strings.NewReader("i"))
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "SHA-256")

	s.refuse = status.Error(codes.AlreadyExists, "other digests")
	err = g.AddBootSet(context.Background(), BootSet{Name: "x"}, strings.NewReader("k"), strings.NewReader("i"))
	assert.ErrorIs(t, err, ErrConflict)

	boom := errors.New("the body broke")
	err = g.AddBootSet(context.Background(), BootSet{Name: "x"}, strings.NewReader("k"), io.MultiReader(strings.NewReader("i"), &failing{boom}))
	assert.ErrorIs(t, err, boom, "the caller's own error comes back as it is")
}

func TestAddBootSet_AStreamMvmdEndsEarlyReportsMvmdsReasonAndNotTheBrokenSend(t *testing.T) {
	s := &bootSetServer{endAtStart: status.Error(codes.InvalidArgument, "the name is not a boot set's")}
	g := serve(t, s)
	big := bytes.NewReader(bytes.Repeat([]byte("k"), 64<<20))
	err := g.AddBootSet(context.Background(), BootSet{Name: "Bad"}, big, strings.NewReader("i"))
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "the name is not")
}

type failing struct{ err error }

func (f *failing) Read([]byte) (int, error) { return 0, f.err }

func TestRemoveBootSet_NamesTheSetAndMapsARefusal(t *testing.T) {
	s := &bootSetServer{}
	g := serve(t, s)
	require.NoError(t, g.RemoveBootSet(context.Background(), "naru-node-v1"))
	assert.Equal(t, []string{"naru-node-v1"}, s.removed)

	s.refuse = status.Error(codes.FailedPrecondition, "a Machine names it")
	assert.ErrorIs(t, g.RemoveBootSet(context.Background(), "naru-node-v1"), ErrPrecondition)
}
