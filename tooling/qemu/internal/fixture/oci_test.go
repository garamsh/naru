package fixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/registry"
)

func TestLayer_HoldsTheEntryExecutable(t *testing.T) {
	layer, err := Layer([]byte("program"))
	require.NoError(t, err)

	tr := tar.NewReader(bytes.NewReader(layer))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, h.Name)
		if h.Name == "bin/entry" {
			body, _ := io.ReadAll(tr)
			assert.Equal(t, "program", string(body))
			assert.Equal(t, int64(0o755), h.Mode)
		}
	}

	assert.Equal(t, []string{"bin", "bin/entry"}, names)
}

// served is a TLS server on the registry material of dir, and a client that trusts only the dir's authority.
func served(t *testing.T, dir string) (*httptest.Server, *http.Client) {
	t.Helper()
	h, err := registry.Handler(dir)
	require.NoError(t, err)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	return s, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func fetch(t *testing.T, c *http.Client, url string) []byte {
	t.Helper()
	res, err := c.Get(url) //nolint:noctx // loopback test server
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode, url)
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return b
}

func TestWriteRegistry_PublishesAPullableImageOverTLS(t *testing.T) {
	dir := t.TempDir()
	img, err := WriteRegistry(dir, []byte("program"), "amd64")
	require.NoError(t, err)
	s, client := served(t, dir)

	manifest := fetch(t, client, s.URL+"/v2/test/entry/manifests/"+img.Digest)

	assert.Equal(t, img.Digest, Digest(manifest), "the image digest is the manifest's")
	assert.Equal(t, RegistryAddr+"/test/entry", img.Repository)
	var m struct {
		Config struct{ Digest string }
		Layers []struct{ Digest string }
	}
	require.NoError(t, json.Unmarshal(manifest, &m))
	require.Len(t, m.Layers, 1)
	layer := fetch(t, client, s.URL+"/v2/test/entry/blobs/"+m.Layers[0].Digest)
	assert.Equal(t, m.Layers[0].Digest, Digest(layer))
	zr, err := gzip.NewReader(bytes.NewReader(layer))
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var cfg struct {
		Architecture string
		Rootfs       struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	require.NoError(t, json.Unmarshal(fetch(t, client, s.URL+"/v2/test/entry/blobs/"+m.Config.Digest), &cfg))
	assert.Equal(t, "amd64", cfg.Architecture)
	assert.Equal(t, []string{Digest(raw)}, cfg.Rootfs.DiffIDs, "the diff id is the digest of the uncompressed layer")
}

func TestWriteRegistry_IsTrustedOnlyThroughItsOwnAuthority(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteRegistry(dir, []byte("program"), "amd64")
	require.NoError(t, err)
	s, _ := served(t, dir)

	_, err = (&http.Client{}).Get(s.URL + "/v2/") //nolint:noctx // loopback test server

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "certificate"), err.Error())
}
