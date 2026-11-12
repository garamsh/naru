package hostapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
)

func TestClient_CSRsListsTheRequestsAndApprovedNeedsTheConditionAndTheCertificate(t *testing.T) {
	certs := cluster(t)
	mux := http.NewServeMux()
	mux.HandleFunc(hostapi.Root+"/certificatesigningrequests", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
{"metadata":{"name":"a"},"spec":{"username":"bootstrap:abcdef"},"status":{"conditions":[{"type":"Approved","status":"True"}],"certificate":"PEM"}},
{"metadata":{"name":"b"},"spec":{"username":"bootstrap:ghijkl"},"status":{"conditions":[{"type":"Approved","status":"True"}]}},
{"metadata":{"name":"c"},"spec":{"username":"node-2"},"status":{"conditions":[{"type":"Denied","status":"True"}],"certificate":"PEM"}}]}`))
	})
	c, err := hostapi.New(serve(t, certs, mux), certs.client)
	require.NoError(t, err)

	got, err := c.CSRs(context.Background())
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, "bootstrap:abcdef", got[0].Spec.Username)
	assert.True(t, got[0].Approved())
	assert.False(t, got[1].Approved(), "approved without a certificate")
	assert.False(t, got[2].Approved(), "denied")
}
