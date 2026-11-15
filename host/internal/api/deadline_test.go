package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// stuckMvmd is an mvmd whose GetRuntimeInfo does not answer until the caller gives up.
type stuckMvmd struct {
	v1alpha2.UnimplementedRuntimeServiceServer
}

func (stuckMvmd) GetRuntimeInfo(ctx context.Context, _ *v1alpha2.GetRuntimeInfoRequest) (*v1alpha2.GetRuntimeInfoResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHost_AnswersAnMvmdThatOutlastsTheCallsDeadlineWith504(t *testing.T) {
	// A unix socket's path is at most 107 bytes, which t.TempDir() under a long test name exceeds.
	dir, err := os.MkdirTemp("", "mvmd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	v1alpha2.RegisterRuntimeServiceServer(srv, stuckMvmd{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	real := mvmd.New(sock)
	t.Cleanup(func() { _ = real.Close() })

	e := start(t)
	// The handler's own bound is 30 s; the test shortens the call it makes to mvmd, whose gRPC client
	// then fails with a status of code DeadlineExceeded.
	e.rt.infoHook = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := real.Info(ctx)
		return err
	}

	status, body := e.do(t, e.as(t, "naru:host-reader"), "GET", "/v1/host", nil)

	assert.Equal(t, http.StatusGatewayTimeout, status, string(body))
}

// cancelledMidAttach makes a request that fails by being cancelled while the host attaches the new
// microVM to its network, and returns once the host has answered the cancelled request.
func cancelledMidAttach(t *testing.T, e *env, method, path string, body any) {
	t.Helper()
	entered := make(chan struct{})
	var once sync.Once
	e.nft.hook = func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+e.addr+path, bytes.NewReader(b))
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := e.as(t, ProvisionerOrganization).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	<-done
	require.Eventually(t, func() bool { e.rt.mu.Lock(); defer e.rt.mu.Unlock(); return len(e.rt.undoErrs) >= 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestCreate_UndoesAMicroVMWhoseRequestWasCancelledWithAContextThatIsNotCancelled(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	req := createBody()
	req["network"] = "a"

	cancelledMidAttach(t, e, "POST", "/v1/microvms", req)

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	assert.NotContains(t, e.rt.machines, "web", "the microVM that was not attached is removed")
	for _, err := range e.rt.undoErrs {
		assert.NoError(t, err, "the undo ran in a cancelled context")
	}
}

func TestResume_UndoesAMicroVMWhoseRequestWasCancelledWithAContextThatIsNotCancelled(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	e.rt.mu.Lock()
	e.rt.undoErrs = nil
	e.rt.mu.Unlock()

	cancelledMidAttach(t, e, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "w2", Network: "a"})

	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	assert.NotContains(t, e.rt.machines, "w2")
	for _, err := range e.rt.undoErrs {
		assert.NoError(t, err, "the undo ran in a cancelled context")
	}
}
