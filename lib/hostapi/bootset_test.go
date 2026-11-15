package hostapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

type part struct{ name, body string }

// bootSetServer answers with status and records the parts of the multipart body in order.
func bootSetServer(t *testing.T, status int) (*hostapi.Client, *[]part) {
	t.Helper()
	var parts []part
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/boot-sets", r.URL.Path)
		mr, err := r.MultipartReader()
		if !assert.NoError(t, err) {
			return
		}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			parts = append(parts, part{p.FormName(), string(b)})
		}
		w.WriteHeader(status)
		if status >= 400 {
			_ = json.NewEncoder(w).Encode(hostapi.ErrorBody{Error: "the name holds other contents"})
		}
	}))
	t.Cleanup(srv.Close)
	return hostapi.NewClient(srv.URL, srv.Client()), &parts
}

func TestAddBootSet_StreamsTheRequestThenTheKernelThenTheInitramfs(t *testing.T) {
	c, parts := bootSetServer(t, http.StatusOK)
	req := hostapi.BootSetRequest{Name: "naru-node", KernelSHA256: "k", InitramfsSHA256: "i"}

	require.NoError(t, c.AddBootSet(t.Context(), req, strings.NewReader("KERNEL"), strings.NewReader("INITRAMFS")))

	require.Len(t, *parts, 3)
	assert.JSONEq(t, `{"name":"naru-node","kernelSha256":"k","initramfsSha256":"i"}`, (*parts)[0].body)
	assert.Equal(t, []part{{"kernel", "KERNEL"}, {"initramfs", "INITRAMFS"}}, (*parts)[1:])
	assert.Equal(t, "boot-set", (*parts)[0].name)
}

func TestAddBootSet_ARefusalIsATypedErrorAndAReadFailureEndsTheUpload(t *testing.T) {
	c, _ := bootSetServer(t, http.StatusConflict)
	err := c.AddBootSet(t.Context(), hostapi.BootSetRequest{Name: "n"}, strings.NewReader("k"), strings.NewReader("i"))
	var apiErr *hostapi.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Status)

	c, _ = bootSetServer(t, http.StatusOK)
	err = c.AddBootSet(t.Context(), hostapi.BootSetRequest{Name: "n"}, strings.NewReader("k"), failingReader{})
	assert.Error(t, err, "a file that cannot be read is not uploaded as a short one")
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRemoveBootSet_DeletesTheNamedSet(t *testing.T) {
	c, got, _ := serve(t, http.StatusNoContent, nil)
	require.NoError(t, c.RemoveBootSet(t.Context(), "naru node"))
	assert.Equal(t, http.MethodDelete, got.Method)
	assert.Equal(t, "/v1/boot-sets/naru%20node", got.URL.EscapedPath())
}

func TestCreateMicroVMReport_TellsAMicroVMMadeNowFromOneAnEarlierRequestMade(t *testing.T) {
	for status, want := range map[int]bool{http.StatusCreated: true, http.StatusOK: false} {
		c, _, _ := serve(t, status, hostapi.MicroVM{Name: "web"})
		m, created, err := c.CreateMicroVMReport(t.Context(), hostapi.CreateRequest{Name: "web"})
		require.NoError(t, err)
		assert.Equal(t, "web", m.Name)
		assert.Equal(t, want, created, "status %d", status)
	}
}
