package orchapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
)

// joinServer answers the calls a joined host's life makes and records what it was sent.
type joinServer struct {
	mu      sync.Mutex
	removed []string
	profile []byte
}

func newJoinClient(t *testing.T) (*orchapi.Client, *joinServer) {
	t.Helper()
	s := &joinServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.removed = append(s.removed, r.PathValue("id"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /v1/profile", func(w http.ResponseWriter, r *http.Request) {
		var req orchapi.HostProfile
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.profile = req.Profile
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/profile", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.profile == nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(orchapi.Error{Message: "no host profile"})
			return
		}
		_ = json.NewEncoder(w).Encode(orchapi.HostProfile{Profile: s.profile})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	c, err := orchapi.New(srv.URL, srv.Client())
	require.NoError(t, err)
	return c, s
}

func TestClient_RemoveDeletesTheHostByItsEscapedID(t *testing.T) {
	c, s := newJoinClient(t)
	require.NoError(t, c.Remove(context.Background(), "h-0123456789abcdef"))
	assert.Equal(t, []string{"h-0123456789abcdef"}, s.removed)
}

func TestClient_TheProfileIsSentAndReadBackAndAMissingOneIsA409(t *testing.T) {
	c, _ := newJoinClient(t)
	ctx := context.Background()

	_, err := c.Profile(ctx)
	var oe *orchapi.Error
	require.ErrorAs(t, err, &oe)
	assert.Equal(t, http.StatusConflict, oe.Status)

	require.NoError(t, c.SetProfile(ctx, []byte("machinePool: 10.200.0.0/16\n")))
	got, err := c.Profile(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("machinePool: 10.200.0.0/16\n"), got.Profile)
}

func TestEnrollStatus_CarriesTheConfigurationAndAPendingHostItsHardware(t *testing.T) {
	var st orchapi.EnrollStatus
	require.NoError(t, json.Unmarshal([]byte(`{"state":"admitted","config":"a2luZDogTm9kZUNvbmZpZwo="}`), &st))
	assert.Equal(t, []byte("kind: NodeConfig\n"), st.Config)

	b, err := json.Marshal(orchapi.EnrollStatus{State: orchapi.StatePending})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "config", "a host that is not admitted is sent no configuration")

	hw := orchapi.Hardware{CPUs: 4, MemoryBytes: 8 << 30,
		Disks: []orchapi.Disk{{Name: "sda", Size: 1 << 40, Model: "SSD", Blank: true}},
		Links: []orchapi.Link{{Name: "eth0", MAC: "52:54:00:00:00:01", Carrier: true}}}
	b, err = json.Marshal(orchapi.EnrollRequest{HostID: "h-1", Hardware: &hw})
	require.NoError(t, err)
	var back orchapi.EnrollRequest
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, &hw, back.Hardware)
}
