// Package fixture makes what a test node needs besides the image: the OCI
// image a Machine runs with the TLS registry material that serves it, and the
// tree of test inputs the tests put on every node's data disk.
package fixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/registry"
)

const (
	manifestType = "application/vnd.oci.image.manifest.v1+json"
	configType   = "application/vnd.oci.image.config.v1+json"
	gzipLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// RegistryAddr is where the registry service listens inside a Node, and the host part of the image's repository.
const RegistryAddr = "127.0.0.1:5000"

// Image is what a PullImage request names.
type Image struct {
	Repository, Digest string
}

// Digest is the "sha256:HEX" digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// Layer is the uncompressed tar archive holding one executable at bin/entry.
func Layer(entry []byte) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	mod := time.Unix(1_700_000_000, 0)
	for _, h := range []*tar.Header{
		{Name: "bin", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mod, Format: tar.FormatUSTAR},
		{Name: "bin/entry", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(entry)), ModTime: mod, Format: tar.FormatUSTAR},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(entry); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func gzipped(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// blobs collects what the registry serves.
type blobs struct {
	dir   string
	types map[string]string
}

func (b *blobs) put(mediaType string, body []byte) (descriptor, error) {
	d := Digest(body)
	b.types[d] = mediaType
	if err := os.WriteFile(filepath.Join(b.dir, d[len("sha256:"):]), body, 0o644); err != nil {
		return descriptor{}, err
	}
	return descriptor{MediaType: mediaType, Digest: d, Size: len(body)}, nil
}

// WriteRegistry writes into dir everything the registry service needs to
// publish an image whose one layer holds the entry executable and whose
// entrypoint is /bin/entry: the blobs, their index, and a certificate and key
// for 127.0.0.1 signed by a new authority, whose certificate it writes as
// ca.pem. It returns the image to pull.
func WriteRegistry(dir string, entry []byte, arch string) (Image, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Image{}, err
	}
	b := &blobs{dir: dir, types: map[string]string{}}
	layer, err := Layer(entry)
	if err != nil {
		return Image{}, err
	}
	compressed, err := gzipped(layer)
	if err != nil {
		return Image{}, err
	}
	layerDesc, err := b.put(gzipLayer, compressed)
	if err != nil {
		return Image{}, err
	}
	cfg, err := json.Marshal(map[string]any{
		"architecture": arch,
		"os":           "linux",
		"config":       map[string]any{"Entrypoint": []string{"/bin/entry"}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{Digest(layer)}},
	})
	if err != nil {
		return Image{}, err
	}
	cfgDesc, err := b.put(configType, cfg)
	if err != nil {
		return Image{}, err
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     manifestType,
		"config":        cfgDesc,
		"layers":        []descriptor{layerDesc},
	})
	if err != nil {
		return Image{}, err
	}
	manifestDesc, err := b.put(manifestType, manifest)
	if err != nil {
		return Image{}, err
	}
	index, err := json.Marshal(b.types)
	if err != nil {
		return Image{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, registry.Index), index, 0o644); err != nil {
		return Image{}, err
	}
	if err := writeCerts(dir); err != nil {
		return Image{}, err
	}
	return Image{Repository: RegistryAddr + "/test/entry", Digest: manifestDesc.Digest}, nil
}

// writeCerts writes ca.pem, cert.pem and key.pem: an authority and a leaf for 127.0.0.1.
func writeCerts(dir string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "naru test registry CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	for name, block := range map[string]*pem.Block{
		"ca.pem":   {Type: "CERTIFICATE", Bytes: caDER},
		"cert.pem": {Type: "CERTIFICATE", Bytes: leafDER},
		"key.pem":  {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
