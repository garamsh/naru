package hostapi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

// serve starts a server that, like gaon's API server, requires a client certificate the cluster's
// CA signed, and serves the paths of handler under the API root.
func serve(t *testing.T, certs *pki.Cluster, handler http.Handler) string {
	t.Helper()
	pair, err := tls.X509KeyPair(certs.APIServer.CertPEM, certs.APIServer.KeyPEM)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certs.CA))
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func cluster(t *testing.T) *pki.Cluster {
	t.Helper()
	c, err := pki.NewCluster([]string{"node-1"}, net.ParseIP("127.0.0.1"))
	require.NoError(t, err)
	return c
}

func TestClient_ReadsNodesAndLeasesOverMutualTLS(t *testing.T) {
	certs := cluster(t)
	mux := http.NewServeMux()
	mux.HandleFunc(hostapi.Root+"/nodes/node-1", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "naru-test-host", r.TLS.PeerCertificates[0].Subject.CommonName, "the server sees the test host's certificate")
		_, _ = w.Write([]byte(`{"metadata":{"name":"node-1"},"status":{"conditions":[{"type":"LeaseExpired","status":"False"},{"type":"Ready","status":"True"}]}}`))
	})
	mux.HandleFunc(hostapi.Root+"/leases/node-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spec":{"holder":"node-1","renewTime":"2026-11-12T01:02:03Z","durationSeconds":30}}`))
	})
	c, err := hostapi.New(serve(t, certs, mux), certs)
	require.NoError(t, err)

	n, err := c.Node(context.Background(), "node-1")
	require.NoError(t, err)
	assert.Equal(t, "node-1", n.Metadata.Name)
	assert.True(t, n.Ready())
	l, err := c.Lease(context.Background(), "node-1")
	require.NoError(t, err)
	assert.Equal(t, "2026-11-12T01:02:03Z", l.Spec.RenewTime.Format("2006-01-02T15:04:05Z"))
}

func TestNode_ReadyIsTheReadyConditionBeingTrue(t *testing.T) {
	var n hostapi.Node
	assert.False(t, n.Ready(), "no condition")
	n.Status.Conditions = []hostapi.Condition{{Type: "LeaseExpired", Status: "True"}}
	assert.False(t, n.Ready(), "another condition")
	n.Status.Conditions = append(n.Status.Conditions, hostapi.Condition{Type: "Ready", Status: "False"})
	assert.False(t, n.Ready(), "Ready false")
	n.Status.Conditions[1].Status = "True"
	assert.True(t, n.Ready())
}

func TestClient_AnswersAMissingResourceWithErrNotFound(t *testing.T) {
	certs := cluster(t)
	c, err := hostapi.New(serve(t, certs, http.NotFoundHandler()), certs)
	require.NoError(t, err)

	_, err = c.Node(context.Background(), "absent")
	assert.ErrorIs(t, err, hostapi.ErrNotFound)
}

func TestClient_CreateNodePutsAnAdmittedNode(t *testing.T) {
	certs := cluster(t)
	var method, path, body string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
	})
	c, err := hostapi.New(serve(t, certs, h), certs)
	require.NoError(t, err)

	require.NoError(t, c.CreateNode(context.Background(), "marker"))
	assert.Equal(t, http.MethodPut, method)
	assert.Equal(t, hostapi.Root+"/nodes/marker", path)
	assert.JSONEq(t, `{"apiVersion":"compute.gaon.garam.sh/v1alpha1","kind":"Node","metadata":{"name":"marker"},"spec":{"admitted":true}}`, body)
}

func TestClient_ASecondCAIsNotTrusted(t *testing.T) {
	served, other := cluster(t), cluster(t)
	c, err := hostapi.New(serve(t, served, http.NotFoundHandler()), other)
	require.NoError(t, err)

	_, err = c.Node(context.Background(), "node-1")
	assert.Error(t, err, "neither side trusts the other's CA")
	assert.NotErrorIs(t, err, hostapi.ErrNotFound)
}
