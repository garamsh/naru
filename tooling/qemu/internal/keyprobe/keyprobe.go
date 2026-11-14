// Package keyprobe finds the files that hold the private half of a given public key, in any PEM
// encoding Go reads (PKCS #8, SEC 1, PKCS #1), so that a test can show no authority key is stored
// unencrypted outside the signer's directory.
package keyprobe

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Public is the PKIX DER of a public key.
type Public string

// Parse reads the certificates and PKIX public keys of PEM.
func Parse(data []byte) ([]Public, error) {
	var out []Public
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		var pub any
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, err
			}
			pub = c.PublicKey
		case "PUBLIC KEY":
			k, err := x509.ParsePKIXPublicKey(b.Bytes)
			if err != nil {
				return nil, err
			}
			pub = k
		default:
			continue
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, err
		}
		out = append(out, Public(der))
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate or public key in the PEM")
	}
	return out, nil
}

// Holds reports whether data holds, in PEM, a private key of one of want.
func Holds(data []byte, want []Public) bool {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return false
		}
		if pub, ok := publicOf(b); ok && slices.Contains(want, pub) {
			return true
		}
	}
}

func publicOf(b *pem.Block) (Public, bool) {
	var key any
	var err error
	switch b.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(b.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(b.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	default:
		return "", false
	}
	if err != nil {
		return "", false
	}
	s, ok := key.(crypto.Signer)
	if !ok {
		return "", false
	}
	der, err := x509.MarshalPKIXPublicKey(s.Public())
	if err != nil {
		return "", false
	}
	return Public(der), true
}

// Scan walks roots, skipping the directories skip, and returns the number of regular files it read
// and the paths of those that hold a private key of one of want. It reads the first MiB of a file.
func Scan(roots, skip []string, want []Public) (int, []string) {
	n := 0
	var found []string
	buf := make([]byte, 1<<20)
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if slices.Contains(skip, p) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer func() { _ = f.Close() }()
			k, _ := f.Read(buf)
			n++
			if strings.Contains(string(buf[:k]), "PRIVATE KEY") && Holds(buf[:k], want) {
				found = append(found, p)
			}
			return nil
		})
	}
	return n, found
}
