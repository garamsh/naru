package signer

import (
	"crypto"
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
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/identity"
)

// The files of the signer's directory, each mode 0600.
const (
	hostsFile        = "hosts.json"   // optional: host ID to the serials of its certificates
	callersFile      = "callers.json" // optional: caller name to the serials of its certificates
	hostCAFile       = "host-ca"
	operatorCAFile   = "operator-ca"
	orchCAFile       = "orch-ca"
	revocationFile   = "revocation.key"
	recoveryPubFile  = "recovery-public.json"
	crlFile          = "crl.json"
	issuedFile       = "issued.json" // optional: a directory made before it has none
	rootCertFile     = "root.crt"    // the first root identity's certificate: Init returns it once, and a crash after Init must not lose it
	certSuffix       = ".crt"
	keySuffix        = ".key"
	bundleVersion    = 1
	notBeforeBackoff = time.Minute
)

// authority is a certificate authority: ECDSA P-256 key and its self-signed certificate.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// signer is what every certificate the authority signs is signed through (ADR 0121): the key in
// the signer's directory now, and a KMS or HSM key behind the same interface later.
func (ca authority) signer() crypto.Signer { return ca.key }

// authorities is everything the signer holds. A value is never changed after it is published
// to the Signer; a change makes a new one.
type authorities struct {
	host, operator, orch authority
	revocation           ed25519.PrivateKey
	recovery             identity.RecoveryPublic
	crl                  identity.CRL
	crlSigned            []byte
	// issued is the role of every serial the signer issued, by serial in lower-case hex. A
	// revoke reads it to keep root's serials root's.
	issued map[string]string
	// hosts lists the certificate serials of every host by host ID, which a rotation revokes.
	hosts map[string][]string
	// callers lists the certificates this signer issued to each caller by name, each with the
	// serial of the one it renewed (ADR 0116).
	callers map[string][]callerRecord
}

// callerRecord is a caller certificate's serial and its parent's: the certificate that signed
// its renewal, empty for a registration or root's issuance.
type callerRecord struct {
	Serial string `json:"serial"`
	Parent string `json:"parent,omitempty"`
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
		NotAfter:              now.AddDate(identity.AuthorityYears, 0, 0),
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

// renew signs the authority's certificate again under its own key, with its subject, for
// identity.AuthorityYears from now. A certificate the old one verified, the new one verifies.
func (ca authority) renew(now time.Time) (authority, error) {
	serial, err := randomSerial()
	if err != nil {
		return authority{}, err
	}
	old := ca.cert
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               old.Subject,
		NotBefore:             now.Add(-notBeforeBackoff),
		NotAfter:              now.AddDate(identity.AuthorityYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              old.KeyUsage,
		SubjectKeyId:          old.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, ca.signer().Public(), ca.signer())
	if err != nil {
		return authority{}, fmt.Errorf("sign %s again: %w", old.Subject.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return authority{}, fmt.Errorf("parse %s certificate: %w", old.Subject.CommonName, err)
	}
	return authority{cert: cert, key: ca.key}, nil
}

// renewed returns a copy of a with each authority whose certificate is at identity.RenewsBy at now
// signed again, and the names of those authorities; none renewed returns a itself.
func (a *authorities) renewed(now time.Time) (*authorities, []string, error) {
	next := *a
	var names []string
	for _, p := range []struct {
		name string
		ca   *authority
	}{{"host", &next.host}, {"operator", &next.operator}, {"orch", &next.orch}} {
		if now.Before(identity.RenewsBy(p.ca.cert)) {
			continue
		}
		renewed, err := p.ca.renew(now)
		if err != nil {
			return nil, nil, err
		}
		*p.ca = renewed
		names = append(names, p.name)
	}
	if len(names) == 0 {
		return a, nil, nil
	}
	return &next, names, nil
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

// save writes the authorities into dir, each file 0600, with the extra files, as one commit: a
// crash leaves the directory as it was, or holds every file. See commitFiles.
func (a *authorities) save(dir string, extra ...stagedFile) error {
	pairs := []struct {
		name string
		ca   authority
	}{{hostCAFile, a.host}, {operatorCAFile, a.operator}, {orchCAFile, a.orch}}
	var files []stagedFile
	for _, p := range pairs {
		keyDER, err := x509.MarshalPKCS8PrivateKey(p.ca.key)
		if err != nil {
			return fmt.Errorf("marshal %s key: %w", p.name, err)
		}
		files = append(files,
			stagedFile{p.name + certSuffix, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.ca.cert.Raw})},
			stagedFile{p.name + keySuffix, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})})
	}
	revDER, err := x509.MarshalPKCS8PrivateKey(a.revocation)
	if err != nil {
		return fmt.Errorf("marshal revocation key: %w", err)
	}
	pub, err := a.recovery.MarshalPublic()
	if err != nil {
		return err
	}
	files = append(files,
		stagedFile{revocationFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: revDER})},
		stagedFile{recoveryPubFile, pub},
		stagedFile{issuedFile, a.marshalIssued()},
		stagedFile{hostsFile, a.marshalHosts()},
		stagedFile{callersFile, a.marshalCallers()},
		stagedFile{crlFile, a.crlSigned})
	return commitFiles(dir, append(files, extra...))
}

func (a *authorities) marshalIssued() []byte {
	b, _ := json.Marshal(a.issued) // a map of strings always marshals
	return b
}

func (a *authorities) marshalHosts() []byte {
	b, _ := json.Marshal(a.hosts) // a map of string slices always marshals
	return b
}

func (a *authorities) marshalCallers() []byte {
	b, _ := json.Marshal(a.callers) // a map of slices of plain records always marshals
	return b
}

// withCaller returns a copy of a that also records serial, issued to role caller, under name, as
// the renewal of parent (empty for none).
func (a *authorities) withCaller(name, serial, parent string) *authorities {
	next := a.withIssued(serial, identity.RoleCaller)
	next.callers = maps.Clone(a.callers)
	if next.callers == nil {
		next.callers = map[string][]callerRecord{}
	}
	next.callers[name] = append(slices.Clone(next.callers[name]), callerRecord{Serial: serial, Parent: parent})
	return next
}

// withHost returns a copy of a that also records serial, issued to role host, under hostID.
func (a *authorities) withHost(hostID, serial string) *authorities {
	next := a.withIssued(serial, identity.RoleHost)
	next.hosts = maps.Clone(a.hosts)
	if next.hosts == nil {
		next.hosts = map[string][]string{}
	}
	next.hosts[hostID] = append(slices.Clone(next.hosts[hostID]), serial)
	return next
}

// withHosts returns a copy of a whose serials for hostID are serials.
func (a *authorities) withHosts(hostID string, serials []string) *authorities {
	next := *a
	next.hosts = maps.Clone(a.hosts)
	next.hosts[hostID] = serials
	return &next
}

// withIssued returns a copy of a that also records serial as issued to role.
func (a *authorities) withIssued(serial string, role identity.Role) *authorities {
	next := *a
	next.issued = maps.Clone(a.issued)
	if next.issued == nil {
		next.issued = map[string]string{}
	}
	next.issued[serial] = string(role)
	return &next
}

// writeFile writes data to path as a file of mode 0600 that is replaced whole or not at all.
func writeFile(path string, data []byte) error {
	if err := atomicfile.Write(path, data, 0o600); err != nil {
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
	pub, err := os.ReadFile(filepath.Join(dir, recoveryPubFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", recoveryPubFile, err)
	}
	if a.recovery, err = identity.ParseRecoveryPublic(pub); err != nil {
		return nil, err
	}
	if a.crlSigned, err = os.ReadFile(filepath.Join(dir, crlFile)); err != nil {
		return nil, fmt.Errorf("read %s: %w", crlFile, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, issuedFile)); err == nil {
		if err := json.Unmarshal(data, &a.issued); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errCorrupt, issuedFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", issuedFile, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, hostsFile)); err == nil {
		if err := json.Unmarshal(data, &a.hosts); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errCorrupt, hostsFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", hostsFile, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, callersFile)); err == nil {
		if err := json.Unmarshal(data, &a.callers); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errCorrupt, callersFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", callersFile, err)
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
	data, err := os.ReadFile(path)
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
	Version    int                       `json:"version"`
	Host       bundleAuthority           `json:"host"`
	Operator   bundleAuthority           `json:"operator"`
	Orch       bundleAuthority           `json:"orch"`
	Revocation []byte                    `json:"revocation"`
	Recovery   identity.RecoveryPublic   `json:"recovery"`
	CRL        []byte                    `json:"crl"`
	Issued     map[string]string         `json:"issued,omitempty"`
	Hosts      map[string][]string       `json:"hosts,omitempty"`
	Callers    map[string][]callerRecord `json:"callers,omitempty"`
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
	b.Revocation, b.Recovery, b.CRL, b.Issued, b.Hosts, b.Callers = revDER, a.recovery, a.crlSigned, a.issued, a.hosts, a.callers
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
	a := &authorities{recovery: b.Recovery, crlSigned: b.CRL, issued: b.Issued, hosts: b.Hosts, callers: b.Callers}
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
