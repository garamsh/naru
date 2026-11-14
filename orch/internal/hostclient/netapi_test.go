package hostclient_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/netrec"
)

// TestDialNetAPISpeaksTheHostAPI runs the real NetAPI against a server that answers as hostd does
// and checks each call's path and body and each answer's mapping.
func TestDialNetAPISpeaksTheHostAPI(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = string(b)
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tunnel":
			_, _ = w.Write([]byte(`{"publicKey":"k","listenPort":51820,"machinePrefixes":["10.200.1.0/24"]}`))
		case "GET /v1/microvms":
			_, _ = w.Write([]byte(`{"microvms":[{"name":"a","network":"lab","podPrefix":"10.9.0.64/26","addresses":["10.9.0.2"]}]}`))
		case "GET /v1/networks":
			_, _ = w.Write([]byte(`{"networks":[{"name":"lab"},{"name":"old"}]}`))
		default:
			_, _ = w.Write([]byte(`{"peers":[]}`))
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	inv := &invFake{hosts: []orchapi.Host{{ID: "h1", Address: addr}}}
	h := hostclient.NewNetHosts(inv, hostclient.DialNetAPI(srv.Client()))

	tun, err := h.Tunnel(t.Context(), "h1")
	require.NoError(t, err)
	assert.Equal(t, netrec.Tunnel{PublicKey: "k", ListenPort: 51820, MachinePrefixes: []string{"10.200.1.0/24"}}, tun)

	vms, err := h.MicroVMs(t.Context(), "h1")
	require.NoError(t, err)
	assert.Equal(t, []netrec.MicroVM{{Name: "a", Network: "lab", PodPrefix: "10.9.0.64/26", Addresses: []string{"10.9.0.2"}}}, vms)

	require.NoError(t, h.PutNetwork(t.Context(), "h1", "lab", netrec.NetworkState{Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2", RemoteMembers: []string{"10.9.0.3/32"}}))
	var nr map[string]any
	require.NoError(t, json.Unmarshal([]byte(got["PUT /v1/networks/lab"]), &nr))
	assert.Equal(t, map[string]any{"prefix": "10.9.0.0/24", "apiEndpoint": "10.9.0.2", "remoteMembers": []any{"10.9.0.3/32"}}, nr)

	names, err := h.Networks(t.Context(), "h1")
	require.NoError(t, err)
	assert.Equal(t, []string{"lab", "old"}, names)
	require.NoError(t, h.DeleteNetwork(t.Context(), "h1", "old"))
	_, deleted := got["DELETE /v1/networks/old"]
	assert.True(t, deleted)

	require.NoError(t, h.PutPeers(t.Context(), "h1", []netrec.Peer{{Name: "h2", PublicKey: "k2", Endpoint: "192.0.2.2:51820", Prefixes: []string{"10.200.2.0/24"}}}))
	var pr map[string]any
	require.NoError(t, json.Unmarshal([]byte(got["PUT /v1/peers"]), &pr))
	assert.Equal(t, map[string]any{"peers": []any{map[string]any{"name": "h2", "publicKey": "k2", "endpoint": "192.0.2.2:51820", "prefixes": []any{"10.200.2.0/24"}}}}, pr)
}
