package command

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	req "github.com/stretchr/testify/require"
)

func TestLoopbackHosts_RefusesAnAddressWithoutAPort(t *testing.T) {
	_, err := loopbackHosts(fakeAddr("no-port"))
	req.Error(t, err, "an address it cannot read would turn the Host guard off")

	hosts, err := loopbackHosts(fakeAddr("127.0.0.1:8484"))
	req.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1:8484", "localhost:8484"}, hosts)
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestAuthorityDigest_IsTheKeyPinOfTheAuthority(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	req.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naru-orch-ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	req.NoError(t, err)
	file := filepath.Join(t.TempDir(), "orch-ca.pem")
	req.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	got, err := authorityDigest(file)
	req.NoError(t, err)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	req.NoError(t, err)
	sum := sha256.Sum256(spki)
	assert.Equal(t, hex.EncodeToString(sum[:]), got, "the seed pins the authority's key, which a re-signing keeps")
}
