package release

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

const registryCAName = "naru test registry CA"

// registryCerts is the authority of the registry the test inputs carry and its serving leaf for
// 127.0.0.1, in the form fixture.ServerCerts makes them, derived from version and epoch alone like
// the test CA (ADR 0078): the keys are HMAC outputs, the certificates are valid from the epoch less
// an hour for ten years, and the signatures are RFC 6979's, since a nil reader makes ecdsa sign so.
func registryCerts(version string, epoch int64) (fixture.Certs, error) {
	if epoch <= 0 {
		return fixture.Certs{}, errors.New("the registry authority's validity needs the epoch (SOURCE_DATE_EPOCH)")
	}
	caKey, err := derivedKeyOf(version, "registry-ca-key")
	if err != nil {
		return fixture.Certs{}, err
	}
	leafKey, err := derivedKeyOf(version, "registry-leaf-key")
	if err != nil {
		return fixture.Certs{}, err
	}
	notBefore := time.Unix(epoch, 0).UTC().Add(-testCASkew)
	notAfter := notBefore.Add(testCAValidity)
	caTemplate := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(derive(version, "registry-ca-serial")[:16]),
		Subject:               pkix.Name{CommonName: registryCAName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(nil, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fixture.Certs{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fixture.Certs{}, err
	}
	ip := net.IPv4(127, 0, 0, 1)
	leafDER, err := x509.CreateCertificate(nil, &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(derive(version, "registry-leaf-serial")[:16]),
		Subject:      pkix.Name{CommonName: ip.String()},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return fixture.Certs{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return fixture.Certs{}, err
	}
	encode := func(typ string, der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}) }
	return fixture.Certs{CA: encode("CERTIFICATE", caDER), Cert: encode("CERTIFICATE", leafDER), Key: encode("EC PRIVATE KEY", keyDER)}, nil
}

// writeRegistryCerts replaces ca.pem, cert.pem and key.pem of the registry directory dir with the
// derived ones.
func writeRegistryCerts(dir, version string, epoch int64) error {
	certs, err := registryCerts(version, epoch)
	if err != nil {
		return err
	}
	for name, content := range map[string][]byte{"ca.pem": certs.CA, "cert.pem": certs.Cert, "key.pem": certs.Key} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}
