package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// The test authority and the bootstrap tokens of the release's user-data are derived from the
// release's version, so that two builds of one commit write the same files (ADR 0078). The
// derivation is no secret: the key is published in naru-node-1.test.yaml, and anyone who knows the
// version can compute it, which ADR 0016 already assumes of it.
const (
	testCAName      = "naru-test-ca"
	testCAValidity  = 10 * 365 * 24 * time.Hour
	testCASkew      = time.Hour
	testTokenLetter = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// testPKI writes the test CA, ca.pem (0644) and ca.key (0600), and one bootstrap token per name,
// tokens/<name> (0600), into dir, in the layout of naructl gen ca and gen token. All of them come
// from version and epoch (SOURCE_DATE_EPOCH, the time the CA is valid from) alone.
func testPKI(dir, version string, epoch int64, names []string) error {
	if epoch <= 0 {
		return errors.New("the test authority's validity needs the epoch (SOURCE_DATE_EPOCH)")
	}
	key, err := derivedKey(version)
	if err != nil {
		return err
	}
	notBefore := time.Unix(epoch, 0).UTC().Add(-testCASkew)
	// The template mirrors pki.New of lib/layout/pki, which naructl gen ca uses, except for the
	// times and the serial. A nil reader makes ecdsa sign as RFC 6979 says, so that one key and one
	// certificate give one signature.
	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(derive(version, "serial")[:16]),
		Subject:               pkix.Name{CommonName: testCAName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(testCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "tokens"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, "tokens", n), []byte(testToken(version, n)+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// derive is 32 bytes fixed by the version and the label.
func derive(version, label string) []byte {
	m := hmac.New(sha256.New, []byte("naru test user-data v1 "+version))
	m.Write([]byte(label))
	return m.Sum(nil)
}

// testToken is the bootstrap token of the Node name, in the form gaon reads: [a-z0-9]{6}.[a-z0-9]{16}.
func testToken(version, name string) string {
	b := derive(version, "token/"+name)
	out := make([]byte, 0, 23)
	for i := 0; i < 22; i++ {
		if i == 6 {
			out = append(out, '.')
		}
		out = append(out, testTokenLetter[int(b[i])%len(testTokenLetter)])
	}
	return string(out)
}

// derivedKey is the P-256 key the version fixes.
func derivedKey(version string) (*ecdsa.PrivateKey, error) {
	n := elliptic.P256().Params().N
	d := new(big.Int).SetBytes(derive(version, "ca-key"))
	d.Mod(d, new(big.Int).Sub(n, big.NewInt(1))).Add(d, big.NewInt(1))
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
}
