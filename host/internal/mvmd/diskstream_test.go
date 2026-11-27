package mvmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const digest64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// diskServer is mvmd's disk stream: ReadDisk answers stream, and CreateDiskFromStream records what
// it was sent and how the stream ended.
type diskServer struct {
	v1alpha2.UnimplementedRuntimeServiceServer
	mu       sync.Mutex
	read     *v1alpha2.ReadDiskRequest
	stream   []*v1alpha2.ReadDiskResponse
	readErr  error
	received []*v1alpha2.CreateDiskFromStreamRequest
	ended    chan error
	createOK error
}

func (s *diskServer) ReadDisk(r *v1alpha2.ReadDiskRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadDiskResponse]) error {
	s.mu.Lock()
	s.read = r
	s.mu.Unlock()
	if s.readErr != nil {
		return s.readErr
	}
	for _, m := range s.stream {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *diskServer) CreateDiskFromStream(stream grpc.ClientStreamingServer[v1alpha2.CreateDiskFromStreamRequest, v1alpha2.CreateDiskFromStreamResponse]) error {
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			s.ended <- nil
			if s.createOK != nil {
				return s.createOK
			}
			return stream.SendAndClose(&v1alpha2.CreateDiskFromStreamResponse{})
		}
		if err != nil {
			s.ended <- err
			return err
		}
		s.mu.Lock()
		s.received = append(s.received, m)
		s.mu.Unlock()
	}
}

func diskGRPC(t *testing.T, srv *diskServer) *GRPC {
	t.Helper()
	srv.ended = make(chan error, 1)
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	v1alpha2.RegisterRuntimeServiceServer(gs, srv)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &GRPC{conn: conn, client: v1alpha2.NewRuntimeServiceClient(conn)}
}

func header(size uint64, fs v1alpha2.Filesystem) *v1alpha2.ReadDiskResponse {
	return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Header{Header: &v1alpha2.DiskStreamHeader{SizeBytes: size, Filesystem: fs}}}
}

func extent(off uint64, data string) *v1alpha2.ReadDiskResponse {
	return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Extent{Extent: &v1alpha2.DiskExtent{Offset: off, Data: []byte(data)}}}
}

func digestMsg(d string) *v1alpha2.ReadDiskResponse {
	return &v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: d}}}
}

func TestReadDisk_PassesTheHeaderTheExtentsAndTheDigestInOrder(t *testing.T) {
	srv := &diskServer{stream: []*v1alpha2.ReadDiskResponse{
		header(4096, v1alpha2.Filesystem_FILESYSTEM_EXT4), extent(0, "ab"), extent(1024, "cd"), digestMsg(digest64),
	}}
	g := diskGRPC(t, srv)
	var got []DiskPart
	require.NoError(t, g.ReadDisk(t.Context(), "m-1.data", func(p DiskPart) error { got = append(got, p); return nil }))
	assert.Equal(t, []DiskPart{
		DiskHeader{SizeBytes: 4096, Filesystem: FilesystemExt4},
		DiskExtent{Offset: 0, Data: []byte("ab")},
		DiskExtent{Offset: 1024, Data: []byte("cd")},
		DiskDigest(digest64),
	}, got)
	assert.Equal(t, "m-1.data", srv.read.GetDiskId())
	assert.Empty(t, srv.read.GetSnapshotId(), "a disk, not a disk snapshot")
}

func TestReadDisk_MapsMvmdsRefusalAndReturnsTheSinksErrorAsItIs(t *testing.T) {
	g := diskGRPC(t, &diskServer{readErr: status.Error(codes.FailedPrecondition, "held by a running Machine")})
	err := g.ReadDisk(t.Context(), "m-1.data", func(DiskPart) error { return nil })
	assert.ErrorIs(t, err, ErrPrecondition)

	stop := status.Error(codes.NotFound, "the sink's own")
	g = diskGRPC(t, &diskServer{stream: []*v1alpha2.ReadDiskResponse{header(1, v1alpha2.Filesystem_FILESYSTEM_NONE)}})
	err = g.ReadDisk(t.Context(), "m-1.data", func(DiskPart) error { return stop })
	assert.Same(t, stop, err, "a sink's error is not read as mvmd's")
}

func TestReadDisk_RefusesAHeaderOfAnUnknownFilesystem(t *testing.T) {
	g := diskGRPC(t, &diskServer{stream: []*v1alpha2.ReadDiskResponse{header(1, v1alpha2.Filesystem_FILESYSTEM_UNSPECIFIED)}})
	err := g.ReadDisk(t.Context(), "m-1.data", func(DiskPart) error { return nil })
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestCreateDiskFromStream_SendsTheStartTheExtentsAndTheDigest(t *testing.T) {
	srv := &diskServer{}
	g := diskGRPC(t, srv)
	err := g.CreateDiskFromStream(t.Context(), "m-2.data", DiskHeader{SizeBytes: 4096, Filesystem: FilesystemNone}, func(send func(DiskPart) error) error {
		if err := send(DiskExtent{Offset: 8, Data: []byte("ab")}); err != nil {
			return err
		}
		return send(DiskDigest(digest64))
	})
	require.NoError(t, err)
	require.Len(t, srv.received, 3)
	start := srv.received[0].GetStart()
	assert.Equal(t, "m-2.data", start.GetDiskId())
	assert.Equal(t, importRequestID, start.GetRequestId())
	assert.Equal(t, uint64(4096), start.GetSizeBytes())
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, start.GetFilesystem())
	assert.Equal(t, uint64(8), srv.received[1].GetExtent().GetOffset())
	assert.Equal(t, []byte("ab"), srv.received[1].GetExtent().GetData())
	assert.Equal(t, digest64, srv.received[2].GetDigest().GetSha256())
}

func TestCreateDiskFromStream_ASendersFailureEndsTheStreamWithoutItsEnd(t *testing.T) {
	srv := &diskServer{}
	g := diskGRPC(t, srv)
	cut := errors.New("the source's stream was cut")
	err := g.CreateDiskFromStream(t.Context(), "m-2.data", DiskHeader{SizeBytes: 4096, Filesystem: FilesystemExt4}, func(send func(DiskPart) error) error {
		if err := send(DiskExtent{Offset: 0, Data: []byte("ab")}); err != nil {
			return err
		}
		return cut
	})
	assert.Same(t, cut, err)
	select {
	case end := <-srv.ended:
		assert.Error(t, end, "the stream is cancelled, not closed, so mvmd never reads it as complete")
	case <-time.After(5 * time.Second):
		t.Fatal("mvmd's stream did not end")
	}
}

func TestCreateDiskFromStream_MapsMvmdsRefusalOfTheBytes(t *testing.T) {
	g := diskGRPC(t, &diskServer{createOK: status.Error(codes.InvalidArgument, "the bytes do not hash to the digest")})
	err := g.CreateDiskFromStream(t.Context(), "m-2.data", DiskHeader{SizeBytes: 1, Filesystem: FilesystemExt4}, func(send func(DiskPart) error) error {
		return send(DiskDigest(digest64))
	})
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestCreateDiskFromStream_RefusesAPartThatIsNotAnExtentOrADigest(t *testing.T) {
	g := diskGRPC(t, &diskServer{})
	err := g.CreateDiskFromStream(t.Context(), "m-2.data", DiskHeader{SizeBytes: 1, Filesystem: FilesystemExt4}, func(send func(DiskPart) error) error {
		return send(DiskHeader{SizeBytes: 1, Filesystem: FilesystemExt4})
	})
	assert.Error(t, err)
	err = g.CreateDiskFromStream(t.Context(), "m-2.data", DiskHeader{SizeBytes: 1, Filesystem: "xfs"}, func(func(DiskPart) error) error { return nil })
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestDiskPart_RoundTripsBetweenHostsAndNoPartIsRefused(t *testing.T) {
	var buf bytes.Buffer
	parts := []DiskPart{
		DiskHeader{SizeBytes: 1 << 30, Filesystem: FilesystemExt4},
		DiskExtent{Offset: 4096, Data: bytes.Repeat([]byte{7}, 3<<20)},
		DiskDigest(digest64),
	}
	for _, p := range parts {
		require.NoError(t, WriteDiskPart(&buf, p))
	}
	r := bufio.NewReader(&buf)
	for _, want := range parts {
		got, err := ReadDiskPart(r)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := ReadDiskPart(r)
	assert.ErrorIs(t, err, io.EOF)

	assert.Error(t, WriteDiskPart(&buf, nil))
	assert.Error(t, WriteDiskPart(&buf, DiskHeader{SizeBytes: 1, Filesystem: "xfs"}))
}
