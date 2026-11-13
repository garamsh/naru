package registry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const digest = "sha256:abc123"

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "abc123"), []byte("blob"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, Index), []byte(`{"`+digest+`":"application/x-test"}`), 0o644))
	h, err := Handler(dir)
	require.NoError(t, err)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(url) //nolint:gosec,noctx // a test server on the loopback
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res, string(b)
}

func TestHandler_ServesManifestsAndBlobsByDigest(t *testing.T) {
	s := serve(t)

	for _, route := range []string{"/v2/test/entry/manifests/", "/v2/test/entry/blobs/"} {
		res, body := get(t, s.URL+route+digest)

		assert.Equal(t, http.StatusOK, res.StatusCode, route)
		assert.Equal(t, "blob", body)
		assert.Equal(t, "application/x-test", res.Header.Get("Content-Type"))
		assert.Equal(t, digest, res.Header.Get("Docker-Content-Digest"))
	}
}

func TestHandler_AnswersTheVersionCheck(t *testing.T) {
	res, _ := get(t, serve(t).URL+"/v2/")

	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestHandler_RefusesWhatItDoesNotHold(t *testing.T) {
	s := serve(t)

	for _, path := range []string{"/v2/x/blobs/sha256:other", "/v2/x/blobs/sha256:../index.json", "/other"} {
		res, _ := get(t, s.URL+path)

		assert.Equal(t, http.StatusNotFound, res.StatusCode, path)
	}
}

func TestHandler_IsReadOnly(t *testing.T) {
	s := serve(t)

	res, err := http.Post(s.URL+"/v2/x/blobs/uploads/", "text/plain", nil) //nolint:gosec,noctx // loopback test server
	require.NoError(t, err)
	_ = res.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
}

func TestBasic_RefusesARequestWithoutTheCredentialAndServesOneWithIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "abc123"), []byte("blob"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, Index), []byte(`{"`+digest+`":"application/x-test"}`), 0o644))
	h, err := Handler(dir)
	require.NoError(t, err)
	s := httptest.NewServer(Basic{User: "u", Password: "p"}.wrap(h))
	t.Cleanup(s.Close)
	url := s.URL + "/v2/test/entry/manifests/" + digest

	res, _ := get(t, url)
	assert.Equal(t, http.StatusUnauthorized, res.StatusCode, "no credential")
	assert.NotEmpty(t, res.Header.Get("Www-Authenticate"))

	for _, pass := range []string{"wrong", "p"} {
		req, err := http.NewRequest(http.MethodGet, url, nil) //nolint:noctx // a test server on the loopback
		require.NoError(t, err)
		req.SetBasicAuth("u", pass)
		got, err := http.DefaultClient.Do(req) //nolint:gosec // a test server on the loopback
		require.NoError(t, err)
		_ = got.Body.Close()
		if pass == "p" {
			assert.Equal(t, http.StatusOK, got.StatusCode)
		} else {
			assert.Equal(t, http.StatusUnauthorized, got.StatusCode, "the wrong password")
		}
	}
}
