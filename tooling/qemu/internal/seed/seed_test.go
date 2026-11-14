package seed_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

// authority writes a Node API authority as naructl gen api-ca does, under dir/api.
func authority(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naru-api-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api", "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api", "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}

func TestAddNodeAPIAddsASpecAPIWhoseServingCertificateVerifiesAgainstTheClientCA(t *testing.T) {
	dir := t.TempDir()
	authority(t, dir)
	file := filepath.Join(dir, "user-data")
	require.NoError(t, os.WriteFile(file, []byte("apiVersion: naru.garam.sh/v1alpha1\nkind: NodeConfig\nspec:\n  hostname: kcp\n"), 0o600))

	require.NoError(t, seed.AddNodeAPI(file, dir, net.ParseIP("10.77.1.2"), net.ParseIP("127.0.0.1")))

	b, err := os.ReadFile(file)
	require.NoError(t, err)
	var got struct {
		Spec struct {
			Hostname string `yaml:"hostname"`
			API      struct {
				ClientCA    string `yaml:"clientCA"`
				Certificate string `yaml:"certificate"`
				Key         string `yaml:"key"`
			} `yaml:"api"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(b, &got))
	assert.Equal(t, "kcp", got.Spec.Hostname, "the rest of the user-data is kept")
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM([]byte(got.Spec.API.ClientCA)))
	cb, _ := pem.Decode([]byte(got.Spec.API.Certificate))
	require.NotNil(t, cb)
	cert, err := x509.ParseCertificate(cb.Bytes)
	require.NoError(t, err)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.NoError(t, err)
	assert.NoError(t, cert.VerifyHostname("10.77.1.2"))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestAddNodeAPIRefusesUserDataThatAlreadyHasAnAPI(t *testing.T) {
	dir := t.TempDir()
	authority(t, dir)
	file := filepath.Join(dir, "user-data")
	require.NoError(t, os.WriteFile(file, []byte("spec:\n  api: {}\n"), 0o600))
	assert.Error(t, seed.AddNodeAPI(file, dir, net.ParseIP("127.0.0.1")))
}
