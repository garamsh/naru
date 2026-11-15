package identity_test

import (
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", dir+"/s.sock")
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir + "/s.sock"
}

func TestSocketSource_ReadsTheListFromTheSignersSocket(t *testing.T) {
	var path string
	sock := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte("signed-list"))
	}))
	b, err := identity.SocketSource{Path: sock}.CRL(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "signed-list", string(b))
	assert.Equal(t, "/v1/crl", path)
}

func TestSocketSource_ErrorsOnAFailureStatusAndOnAMissingSocket(t *testing.T) {
	sock := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
	_, err := identity.SocketSource{Path: sock}.CRL(t.Context())
	assert.Error(t, err)
	_, err = identity.SocketSource{Path: sock + ".missing"}.CRL(t.Context())
	assert.Error(t, err)
}

func TestParseRevocationPublic_RoundTripsAndRefusesWhatIsNotAnEd25519Key(t *testing.T) {
	r := newRig(t)
	pemKey, err := identity.EncodeRevocationPublic(r.pub)
	require.NoError(t, err)
	got, err := identity.ParseRevocationPublic([]byte(pemKey))
	require.NoError(t, err)
	assert.True(t, r.pub.Equal(got))
	_, err = identity.ParseRevocationPublic([]byte("not pem"))
	assert.Error(t, err)
}
