package release

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// The test authority and the bootstrap tokens of the release's user-data are derived from the
// release's version, so that two builds of one commit write the same files (ADR 0076). The
// derivation is no secret: the key is published in naru-node-1.test.yaml, and anyone who knows the
// version can compute it, which the ADR 0016 already assumes of it.
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
	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(derive(version, "serial")[:16]),
		Subject:               pkix.Name{CommonName: testCAName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(testCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, &key.PublicKey, deterministicSigner{key})
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

// deterministicSigner signs as RFC 6979 says, so that one key and one certificate give one
// signature: crypto/ecdsa mixes fresh randomness into every signature, whatever reader it is given.
type deterministicSigner struct{ key *ecdsa.PrivateKey }

func (s deterministicSigner) Public() crypto.PublicKey { return s.key.Public() }

func (s deterministicSigner) Sign(_ io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	n := elliptic.P256().Params().N
	x := s.key.D.FillBytes(make([]byte, 32))
	h1 := new(big.Int).SetBytes(digest)
	if bits := len(digest) * 8; bits > n.BitLen() {
		h1.Rsh(h1, uint(bits-n.BitLen()))
	}
	h1.Mod(h1, n)
	hb := h1.FillBytes(make([]byte, 32))

	mac := func(k []byte, parts ...[]byte) []byte {
		m := hmac.New(sha256.New, k)
		for _, p := range parts {
			m.Write(p)
		}
		return m.Sum(nil)
	}
	v := make([]byte, 32)
	k := make([]byte, 32)
	for i := range v {
		v[i] = 1
	}
	k = mac(k, v, []byte{0}, x, hb)
	v = mac(k, v)
	k = mac(k, v, []byte{1}, x, hb)
	v = mac(k, v)
	for {
		v = mac(k, v)
		kk := new(big.Int).SetBytes(v)
		if kk.Sign() > 0 && kk.Cmp(n) < 0 {
			pub, err := ecdh.P256().NewPrivateKey(v)
			if err != nil {
				return nil, err
			}
			r := new(big.Int).SetBytes(pub.PublicKey().Bytes()[1:33])
			r.Mod(r, n)
			if r.Sign() != 0 {
				sv := new(big.Int).Mul(r, s.key.D)
				sv.Add(sv, h1).Mul(sv, new(big.Int).ModInverse(kk, n)).Mod(sv, n)
				if sv.Sign() != 0 {
					return asn1.Marshal(struct{ R, S *big.Int }{r, sv})
				}
			}
		}
		k = mac(k, v, []byte{0})
		v = mac(k, v)
	}
}
