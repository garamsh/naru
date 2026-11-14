package signer

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// The files of the signer's directory, each mode 0600.
const (
	hostCAFile       = "host-ca"
	operatorCAFile   = "operator-ca"
	orchCAFile       = "orch-ca"
	revocationFile   = "revocation.key"
	recoveryPubFile  = "recovery-public.json"
	crlFile          = "crl.json"
	certSuffix       = ".crt"
	keySuffix        = ".key"
	bundleVersion    = 1
	authorityYears   = 10
	notBeforeBackoff = time.Minute
)

// authority is a certificate authority: ECDSA P-256 key and its self-signed certificate.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// authorities is everything the signer holds. A value is never changed after it is published
// to the Signer; a change makes a new one.
type authorities struct {
	host, operator, orch authority
	revocation           ed25519.PrivateKey
	recovery             identity.RecoveryPublic
	crl                  identity.CRL
	crlSigned            []byte
}

func newAuthority(cn string, now time.Time) (authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return authority{}, fmt.Errorf("generate %s key: %w", cn, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return authority{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-notBeforeBackoff),
		NotAfter:              now.AddDate(authorityYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return authority{}, fmt.Errorf("create %s certificate: %w", cn, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return authority{}, fmt.Errorf("parse %s certificate: %w", cn, err)
	}
	return authority{cert: cert, key: key}, nil
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

// newAuthorities makes every authority and an empty revocation list.
func newAuthorities(recovery identity.RecoveryPublic, now time.Time) (*authorities, error) {
	a := &authorities{recovery: recovery}
	var err error
	if a.host, err = newAuthority("naru-host-ca", now); err != nil {
		return nil, err
	}
	if a.operator, err = newAuthority("naru-operator-ca", now); err != nil {
		return nil, err
	}
	if a.orch, err = newAuthority("naru-orch-ca", now); err != nil {
		return nil, err
	}
	if _, a.revocation, err = ed25519.GenerateKey(rand.Reader); err != nil {
		return nil, fmt.Errorf("generate revocation key: %w", err)
	}
	if err := a.signCRL(identity.CRL{Seq: 1, IssuedAt: now, Serials: []string{}}); err != nil {
		return nil, err
	}
	return a, nil
}

// signCRL makes crl the list a holds, signed.
func (a *authorities) signCRL(crl identity.CRL) error {
	signed, err := identity.SignCRL(a.revocation, crl)
	if err != nil {
		return err
	}
	a.crl, a.crlSigned = crl, signed
	return nil
}

func (a *authorities) operatorPool() *x509.CertPool { return pool(a.operator.cert) }

// check refuses authorities that do not hold together: a key that is not its certificate's, a
// certificate that is not a CA, or a revocation list the revocation key did not sign.
func (a *authorities) check() error {
	for name, ca := range map[string]authority{"host": a.host, "operator": a.operator, "orch": a.orch} {
		if ca.cert == nil || ca.key == nil || !ca.cert.IsCA || !ca.key.PublicKey.Equal(ca.cert.PublicKey) {
			return fmt.Errorf("%w: the %s authority is not a certificate with its key", errCorrupt, name)
		}
	}
	crl, err := identity.VerifyCRL(a.revocation.Public().(ed25519.PublicKey), a.crlSigned)
	if err != nil {
		return fmt.Errorf("%w: %w", errCorrupt, err)
	}
	a.crl = *crl
	if _, err := a.recovery.SealKey(); err != nil {
		return fmt.Errorf("%w: %w", errCorrupt, err)
	}
	return nil
}

var errCorrupt = errors.New("authorities are damaged")

// authorityFiles lists every file that makes the authorities.
func authorityFiles() []string {
	return []string{
		hostCAFile + certSuffix, hostCAFile + keySuffix,
		operatorCAFile + certSuffix, operatorCAFile + keySuffix,
		orchCAFile + certSuffix, orchCAFile + keySuffix,
		revocationFile, recoveryPubFile, crlFile,
	}
}

// save writes the authorities into dir, each file 0600. The list goes last, so a directory that
// holds it holds everything.
func (a *authorities) save(dir string) error {
	pairs := []struct {
		name string
		ca   authority
	}{{hostCAFile, a.host}, {operatorCAFile, a.operator}, {orchCAFile, a.orch}}
	for _, p := range pairs {
		keyDER, err := x509.MarshalPKCS8PrivateKey(p.ca.key)
		if err != nil {
			return fmt.Errorf("marshal %s key: %w", p.name, err)
		}
		if err := writeFile(filepath.Join(dir, p.name+certSuffix), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.ca.cert.Raw})); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, p.name+keySuffix), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
			return err
		}
	}
	revDER, err := x509.MarshalPKCS8PrivateKey(a.revocation)
	if err != nil {
		return fmt.Errorf("marshal revocation key: %w", err)
	}
	if err := writeFile(filepath.Join(dir, revocationFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: revDER})); err != nil {
		return err
	}
	pub, err := a.recovery.MarshalPublic()
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, recoveryPubFile), pub); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, crlFile), a.crlSigned)
}

// writeFile writes data to path as a file of mode 0600 that is replaced whole or not at all.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// loadAuthorities reads dir: nil for a directory with none of the files, an error for some.
func loadAuthorities(dir string) (*authorities, error) {
	var found int
	for _, name := range authorityFiles() {
		info, err := os.Stat(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("%w: %w", ErrDirectory, err)
		case info.Mode().Perm()&0o077 != 0:
			return nil, fmt.Errorf("%w: %s has mode %04o, not 0600", ErrDirectory, name, info.Mode().Perm())
		default:
			found++
		}
	}
	if found == 0 {
		return nil, nil
	}
	if found != len(authorityFiles()) {
		return nil, fmt.Errorf("%w: %d of %d authority files are present", errCorrupt, found, len(authorityFiles()))
	}
	a := &authorities{}
	var err error
	for _, p := range []struct {
		name string
		to   *authority
	}{{hostCAFile, &a.host}, {operatorCAFile, &a.operator}, {orchCAFile, &a.orch}} {
		if *p.to, err = readAuthority(dir, p.name); err != nil {
			return nil, err
		}
	}
	revDER, err := readPEM(filepath.Join(dir, revocationFile), "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	rev, err := x509.ParsePKCS8PrivateKey(revDER)
	if err != nil {
		return nil, fmt.Errorf("%w: revocation key: %w", errCorrupt, err)
	}
	var ok bool
	if a.revocation, ok = rev.(ed25519.PrivateKey); !ok {
		return nil, fmt.Errorf("%w: revocation key is not Ed25519", errCorrupt)
	}
	pub, err := os.ReadFile(filepath.Join(dir, recoveryPubFile)) //nolint:gosec // a file of the signer's own directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", recoveryPubFile, err)
	}
	if a.recovery, err = identity.ParseRecoveryPublic(pub); err != nil {
		return nil, err
	}
	if a.crlSigned, err = os.ReadFile(filepath.Join(dir, crlFile)); err != nil { //nolint:gosec // a file of the signer's own directory
		return nil, fmt.Errorf("read %s: %w", crlFile, err)
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	return a, nil
}

func readAuthority(dir, name string) (authority, error) {
	certDER, err := readPEM(filepath.Join(dir, name+certSuffix), "CERTIFICATE")
	if err != nil {
		return authority{}, err
	}
	keyDER, err := readPEM(filepath.Join(dir, name+keySuffix), "PRIVATE KEY")
	if err != nil {
		return authority{}, err
	}
	return parseAuthority(certDER, keyDER)
}

func parseAuthority(certDER, keyDER []byte) (authority, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return authority{}, fmt.Errorf("%w: certificate: %w", errCorrupt, err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return authority{}, fmt.Errorf("%w: key: %w", errCorrupt, err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return authority{}, fmt.Errorf("%w: key is not ECDSA", errCorrupt)
	}
	return authority{cert: cert, key: key}, nil
}

func readPEM(path, blockType string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a file of the signer's own directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != blockType {
		return nil, fmt.Errorf("%w: %s holds no %s", errCorrupt, path, blockType)
	}
	return block.Bytes, nil
}

// bundleFile is the plaintext of a bundle: every authority key, with what is needed to use them.
type bundleFile struct {
	Version    int                     `json:"version"`
	Host       bundleAuthority         `json:"host"`
	Operator   bundleAuthority         `json:"operator"`
	Orch       bundleAuthority         `json:"orch"`
	Revocation []byte                  `json:"revocation"`
	Recovery   identity.RecoveryPublic `json:"recovery"`
	CRL        []byte                  `json:"crl"`
}

type bundleAuthority struct {
	Certificate []byte `json:"certificate"`
	Key         []byte `json:"key"`
}

// marshalBundle returns the plaintext of the bundle of a.
func (a *authorities) marshalBundle() ([]byte, error) {
	var b bundleFile
	b.Version = bundleVersion
	for _, p := range []struct {
		from authority
		to   *bundleAuthority
	}{{a.host, &b.Host}, {a.operator, &b.Operator}, {a.orch, &b.Orch}} {
		keyDER, err := x509.MarshalPKCS8PrivateKey(p.from.key)
		if err != nil {
			return nil, fmt.Errorf("marshal key: %w", err)
		}
		*p.to = bundleAuthority{Certificate: p.from.cert.Raw, Key: keyDER}
	}
	revDER, err := x509.MarshalPKCS8PrivateKey(a.revocation)
	if err != nil {
		return nil, fmt.Errorf("marshal revocation key: %w", err)
	}
	b.Revocation, b.Recovery, b.CRL = revDER, a.recovery, a.crlSigned
	return json.Marshal(b)
}

// parseBundle reads what marshalBundle wrote and checks the authorities hold together.
func parseBundle(plain []byte) (*authorities, error) {
	var b bundleFile
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, fmt.Errorf("%w: %w", errCorrupt, err)
	}
	if b.Version != bundleVersion {
		return nil, fmt.Errorf("%w: bundle version %d", errCorrupt, b.Version)
	}
	a := &authorities{recovery: b.Recovery, crlSigned: b.CRL}
	var err error
	for _, p := range []struct {
		from bundleAuthority
		to   *authority
	}{{b.Host, &a.host}, {b.Operator, &a.operator}, {b.Orch, &a.orch}} {
		if *p.to, err = parseAuthority(p.from.Certificate, p.from.Key); err != nil {
			return nil, err
		}
	}
	rev, err := x509.ParsePKCS8PrivateKey(b.Revocation)
	if err != nil {
		return nil, fmt.Errorf("%w: revocation key: %w", errCorrupt, err)
	}
	var ok bool
	if a.revocation, ok = rev.(ed25519.PrivateKey); !ok {
		return nil, fmt.Errorf("%w: revocation key is not Ed25519", errCorrupt)
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	return a, nil
}
