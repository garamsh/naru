package mvmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// guestServer is mvmd's guest calls: it records what it was sent and answers what the test set.
type guestServer struct {
	v1alpha2.UnimplementedRuntimeServiceServer
	mu      sync.Mutex
	exec    *v1alpha2.ExecRequest
	signal  *v1alpha2.SignalExecRequest
	output  []*v1alpha2.ReadExecOutputResponse
	file    map[string][]byte
	start   *v1alpha2.WriteFileStart
	chunks  int
	written []byte
}

func (s *guestServer) Exec(_ context.Context, r *v1alpha2.ExecRequest) (*v1alpha2.ExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = r
	if r.GetCommand()[0] == "absent" {
		return &v1alpha2.ExecResponse{Record: &v1alpha2.ExecRecord{ExecId: r.GetExecId(), Status: v1alpha2.ExecStatus_EXEC_STATUS_NOT_STARTED, Reason: "absent: not found"}}, nil
	}
	return &v1alpha2.ExecResponse{Record: &v1alpha2.ExecRecord{ExecId: r.GetExecId(), Status: v1alpha2.ExecStatus_EXEC_STATUS_STARTED}}, nil
}

func (s *guestServer) ReadExecOutput(_ *v1alpha2.ReadExecOutputRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadExecOutputResponse]) error {
	for _, m := range s.output {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *guestServer) SignalExec(_ context.Context, r *v1alpha2.SignalExecRequest) (*v1alpha2.SignalExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signal = r
	return &v1alpha2.SignalExecResponse{}, nil
}

func (s *guestServer) ReadFile(r *v1alpha2.ReadFileRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadFileResponse]) error {
	b, ok := s.file[r.GetPath()]
	if !ok {
		return status.Error(codes.NotFound, "no such file")
	}
	for len(b) > 0 {
		n := min(len(b), 3)
		if err := stream.Send(&v1alpha2.ReadFileResponse{Chunk: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (s *guestServer) WriteFile(stream grpc.ClientStreamingServer[v1alpha2.WriteFileRequest, v1alpha2.WriteFileResponse]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&v1alpha2.WriteFileResponse{})
		}
		if err != nil {
			return err
		}
		if st := m.GetStart(); st != nil {
			s.start = st
			continue
		}
		s.chunks++
		s.written = append(s.written, m.GetChunk()...)
	}
}

// guestGRPC is a GRPC whose connection is to srv, in process.
func guestGRPC(t *testing.T, srv *guestServer) *GRPC {
	t.Helper()
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

func TestExec_StartsTheCommandUnderItsOwnIDOnceAndReadsWhetherItStarted(t *testing.T) {
	srv := &guestServer{}
	g := guestGRPC(t, srv)
	rec, err := g.Exec(t.Context(), "m-1", "x-1", []string{"cat", "/data/a"})
	require.NoError(t, err)
	assert.Equal(t, ExecStarted, rec.Status)
	assert.Equal(t, "m-1", srv.exec.GetMachineId())
	assert.Equal(t, "x-1", srv.exec.GetExecId())
	assert.Equal(t, "x-1", srv.exec.GetRequestId(), "the exec's ID is its request ID, so a repeat runs nothing twice")
	assert.Equal(t, []string{"cat", "/data/a"}, srv.exec.GetCommand())

	rec, err = g.Exec(t.Context(), "m-1", "x-2", []string{"absent"})
	require.NoError(t, err)
	assert.Equal(t, ExecRecord{Status: ExecNotStarted, Reason: "absent: not found"}, rec)
}

func TestReadExecOutput_PassesEachChunkWithItsStreamAndReturnsTheRecord(t *testing.T) {
	chunk := func(stream v1alpha2.OutputStream, offset uint64, data string) *v1alpha2.ReadExecOutputResponse {
		return &v1alpha2.ReadExecOutputResponse{Part: &v1alpha2.ReadExecOutputResponse_Chunk{Chunk: &v1alpha2.OutputChunk{Stream: stream, Offset: offset, Data: []byte(data)}}}
	}
	srv := &guestServer{output: []*v1alpha2.ReadExecOutputResponse{
		chunk(v1alpha2.OutputStream_OUTPUT_STREAM_STDOUT, 0, "out"),
		chunk(v1alpha2.OutputStream_OUTPUT_STREAM_STDERR, 3, "err"),
		{Part: &v1alpha2.ReadExecOutputResponse_Record{Record: &v1alpha2.ExecRecord{Status: v1alpha2.ExecStatus_EXEC_STATUS_EXITED, ExitCode: 137, TermSignal: 9}}},
	}}
	g := guestGRPC(t, srv)
	var got []Chunk
	rec, err := g.ReadExecOutput(t.Context(), "m-1", "x-1", 0, func(c Chunk) error { got = append(got, c); return nil })
	require.NoError(t, err)
	assert.Equal(t, []Chunk{{Offset: 0, Data: []byte("out")}, {Stderr: true, Offset: 3, Data: []byte("err")}}, got)
	assert.Equal(t, ExecRecord{Status: ExecExited, ExitCode: 137, TermSignal: 9}, rec)

	full := errors.New("full")
	_, err = g.ReadExecOutput(t.Context(), "m-1", "x-1", 0, func(Chunk) error { return full })
	assert.ErrorIs(t, err, full, "the sink's error ends the read as it is")
}

func TestSignalExec_SendsTheSignalToTheExec(t *testing.T) {
	srv := &guestServer{}
	require.NoError(t, guestGRPC(t, srv).SignalExec(t.Context(), "m-1", "x-1", 9))
	assert.Equal(t, "m-1", srv.signal.GetMachineId())
	assert.Equal(t, "x-1", srv.signal.GetExecId())
	assert.Equal(t, int32(9), srv.signal.GetSignal())
}

func TestReadFile_PassesTheChunksInOrderAndAnAbsentFileIsNotFound(t *testing.T) {
	srv := &guestServer{file: map[string][]byte{"/data/a": []byte("hello world")}}
	g := guestGRPC(t, srv)
	var got []byte
	require.NoError(t, g.ReadFile(t.Context(), "m-1", "/data/a", func(b []byte) error { got = append(got, b...); return nil }))
	assert.Equal(t, "hello world", string(got))
	assert.ErrorIs(t, g.ReadFile(t.Context(), "m-1", "/data/b", func([]byte) error { return nil }), ErrNotFound)
	full := errors.New("full")
	assert.ErrorIs(t, g.ReadFile(t.Context(), "m-1", "/data/a", func([]byte) error { return full }), full)
}

func TestWriteFile_SendsTheStartThenTheContentInChunks(t *testing.T) {
	srv := &guestServer{}
	g := guestGRPC(t, srv)
	content := bytes.Repeat([]byte("x"), 2*fileChunk+1)
	require.NoError(t, g.WriteFile(t.Context(), "m-1", "/data/a", bytes.NewReader(content)))
	assert.Equal(t, "m-1", srv.start.GetMachineId())
	assert.Equal(t, "/data/a", srv.start.GetPath())
	assert.Equal(t, 3, srv.chunks, "no message holds more than one chunk")
	assert.Equal(t, content, srv.written)

	require.NoError(t, g.WriteFile(t.Context(), "m-1", "/data/empty", bytes.NewReader(nil)))
	assert.Equal(t, "/data/empty", srv.start.GetPath(), "an empty file is its start alone")
}
