// Package pki issues the certificates spec.pki asks for, from the authorities
// the configuration carries, and renews them when two thirds of their
// validity has passed (docs/architecture/adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md).
package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/garamsh/naru/init/internal/config"
)

const (
	// backdate is how far before issuance a certificate's validity starts, so
	// a peer whose clock is behind still accepts it.
	backdate = time.Hour
	// retryAfter is how long init waits before it retries a renewal that failed.
	retryAfter = time.Minute
)

// Clock is the time init issues and renews by.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Restarter restarts a running service by name.
type Restarter interface {
	Restart(name string)
}

// Issuer issues the certificates of a spec.pki and renews them.
type Issuer struct {
	root      string
	clock     Clock
	restarter Restarter
	log       *slog.Logger

	spec   config.PKI
	addrs  []netip.Addr
	issued []*issued
}

// issued is one certificate of the spec as it was last issued.
type issued struct {
	cert config.Certificate
	due  time.Time
}

// New returns an Issuer that writes the paths of the configuration under
// root, which is empty on a Node.
func New(root string, clock Clock, restarter Restarter, log *slog.Logger) *Issuer {
	return &Issuer{root: root, clock: clock, restarter: restarter, log: log}
}

// Issue writes the certificates of the authorities and issues the
// certificates of spec, with addrs as the addresses nodeAddresses stands for.
// It refuses, and leaves the remaining certificates unissued, at the first
// failure.
func (i *Issuer) Issue(spec config.PKI, addrs []netip.Addr) (err error) {
	i.spec, i.addrs, i.issued = spec, addrs, nil
	// After a refusal Run has nothing to renew: the boot it belongs to starts no service.
	defer func() {
		if err != nil {
			i.issued = nil
		}
	}()
	for _, a := range spec.Authorities {
		if err := i.create(a.CertificatePath, []byte(a.Certificate), 0o644); err != nil {
			return fmt.Errorf("write authority %s: %w", a.Name, err)
		}
		if a.KeyPath != "" {
			if err := i.create(a.KeyPath, []byte(a.Key), 0o600); err != nil {
				return fmt.Errorf("write the key of authority %s: %w", a.Name, err)
			}
		}
		i.log.Info("authority written", "name", a.Name, "path", a.CertificatePath, "key", a.KeyPath != "")
	}
	for _, c := range spec.Certificates {
		now := i.clock.Now()
		certPEM, keyPEM, expires, err := i.sign(c, now)
		if err != nil {
			return fmt.Errorf("issue certificate %s: %w", c.Name, err)
		}
		if err := i.create(c.KeyPath, keyPEM, 0o600); err != nil {
			return fmt.Errorf("write the key of certificate %s: %w", c.Name, err)
		}
		if err := i.create(c.CertificatePath, certPEM, 0o644); err != nil {
			return fmt.Errorf("write certificate %s: %w", c.Name, err)
		}
		i.issued = append(i.issued, &issued{cert: c, due: dueAt(now, expires.Sub(now))})
		i.log.Info("certificate issued", "name", c.Name, "authority", c.Authority, "usage", string(c.Usage), "path", c.CertificatePath, "expires", expires.UTC().Format(time.RFC3339))
	}
	return nil
}

// Run renews each issued certificate when two thirds of its life has passed,
// until ctx ends, and returns at once when none was issued. A renewal that
// fails is logged and retried. It must not run while Issue does.
func (i *Issuer) Run(ctx context.Context) {
	for len(i.issued) > 0 {
		next := i.issued[0].due
		for _, s := range i.issued[1:] {
			if s.due.Before(next) {
				next = s.due
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-i.clock.After(next.Sub(i.clock.Now())):
		}
		i.renewDue(ctx)
	}
}

func (i *Issuer) renewDue(ctx context.Context) {
	now := i.clock.Now()
	var restart []string
	named := map[string]bool{}
	for _, s := range i.issued {
		if ctx.Err() != nil {
			return
		}
		if s.due.After(now) {
			continue
		}
		certPEM, keyPEM, expires, err := i.sign(s.cert, now)
		if err == nil {
			err = i.replace(s.cert, certPEM, keyPEM)
		}
		if err != nil {
			s.due = now.Add(retryAfter)
			i.log.Error("certificate renewal failed", "name", s.cert.Name, "err", err, "retry", retryAfter.String())
			continue
		}
		s.due = dueAt(now, expires.Sub(now))
		i.log.Info("certificate renewed", "name", s.cert.Name, "authority", s.cert.Authority, "path", s.cert.CertificatePath, "expires", expires.UTC().Format(time.RFC3339))
		for _, name := range s.cert.Restart {
			if !named[name] {
				named[name] = true
				restart = append(restart, name)
			}
		}
	}
	// Once per service, after every renewal of the pass, so a service that two
	// certificates name restarts once and reads both new pairs.
	for _, name := range restart {
		if ctx.Err() != nil {
			return
		}
		i.restarter.Restart(name)
	}
}

// dueAt is when a certificate issued at now, with a life of d, is renewed: two
// thirds of d, taken as d/3*2 because d*2 overflows for a d over about 146 years.
func dueAt(now time.Time, d time.Duration) time.Time {
	return now.Add(d / 3 * 2)
}

// sign returns a new certificate for c, issued at now, with a new key, both in
// PEM, and when it expires: its validity from now, or the authority's expiry
// when that comes first. It refuses a certificate that would live less than
// config.MinValidity.
func (i *Issuer) sign(c config.Certificate, now time.Time) (certPEM, keyPEM []byte, expires time.Time, err error) {
	auth, ok := i.spec.Authority(c.Authority)
	if !ok {
		return nil, nil, time.Time{}, fmt.Errorf("authority %q is not in spec.pki", c.Authority)
	}
	signer, err := auth.Signer()
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("authority %s: %w", c.Authority, err)
	}
	if now.Before(signer.Certificate.NotBefore) || now.After(signer.Certificate.NotAfter) {
		return nil, nil, time.Time{}, fmt.Errorf("authority %s is not valid at %s", c.Authority, now.UTC().Format(time.RFC3339))
	}
	expires = now.Add(c.ValidFor())
	if notAfter := signer.Certificate.NotAfter; notAfter.Before(expires) {
		expires = notAfter
		i.log.Warn("authority expires soon", "name", c.Authority, "expires", notAfter.UTC().Format(time.RFC3339), "certificate", c.Name)
		if expires.Sub(now) < config.MinValidity {
			return nil, nil, time.Time{}, fmt.Errorf("authority %s expires at %s, which leaves less than %s for certificate %s", c.Authority, notAfter.UTC().Format(time.RFC3339), config.MinValidity, c.Name)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	subject := pkix.Name{CommonName: c.CommonName}
	if c.Organization != "" {
		subject.Organization = []string{c.Organization}
	}
	usage := x509.ExtKeyUsageClientAuth
	if c.Usage == config.Server {
		usage = x509.ExtKeyUsageServerAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    now.Add(-backdate),
		NotAfter:     expires,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     c.DNSNames,
		IPAddresses:  i.ipAddresses(c),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer.Certificate, &key.PublicKey, signer.Key)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, expires, nil
}

// ipAddresses returns the addresses c names, each once, in a stable order.
func (i *Issuer) ipAddresses(c config.Certificate) []net.IP {
	set := map[netip.Addr]bool{}
	for _, s := range c.IPAddresses {
		if a, err := netip.ParseAddr(s); err == nil {
			set[a.Unmap()] = true
		}
	}
	if c.NodeAddresses {
		for _, a := range i.addrs {
			set[a.Unmap()] = true
		}
		if c.Usage == config.Server {
			set[netip.MustParseAddr("127.0.0.1")] = true
		}
	}
	addrs := make([]netip.Addr, 0, len(set))
	for a := range set {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(x, y int) bool { return addrs[x].Less(addrs[y]) })
	ips := make([]net.IP, len(addrs))
	for n, a := range addrs {
		ips[n] = net.IP(a.AsSlice())
	}
	return ips
}

// create writes data to a path that does not exist yet, with exactly perm.
func (i *Issuer) create(path string, data []byte, perm os.FileMode) error {
	p := filepath.Join(i.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	return errors.Join(err, f.Close())
}

// replace swaps in a renewed key and certificate. It writes both beside their
// paths first, and renames only once both are written, the key first. When the
// certificate's rename fails it puts the old key back, from a link made before
// the key was replaced, so a failure leaves the old pair. What it cannot close
// is a crash between the two renames, and a failure to put the old key back;
// the error says so for the latter.
func (i *Issuer) replace(c config.Certificate, certPEM, keyPEM []byte) (err error) {
	keyPath, certPath := filepath.Join(i.root, c.KeyPath), filepath.Join(i.root, c.CertificatePath)
	keyTmp, err := stage(keyPath, keyPEM, 0o600)
	if err != nil {
		return fmt.Errorf("write the new key: %w", err)
	}
	defer func() { _ = os.Remove(keyTmp) }()
	certTmp, err := stage(certPath, certPEM, 0o644)
	if err != nil {
		return fmt.Errorf("write the new certificate: %w", err)
	}
	defer func() { _ = os.Remove(certTmp) }()

	oldKey := keyTmp + ".old"
	if err := os.Link(keyPath, oldKey); err != nil {
		return fmt.Errorf("keep the old key: %w", err)
	}
	defer func() { _ = os.Remove(oldKey) }()
	if err := os.Rename(keyTmp, keyPath); err != nil {
		return fmt.Errorf("replace the key: %w", err)
	}
	if err := os.Rename(certTmp, certPath); err != nil {
		err = fmt.Errorf("replace the certificate: %w", err)
		if back := os.Rename(oldKey, keyPath); back != nil {
			return errors.Join(err, fmt.Errorf("put the old key back, so the new key stands beside the old certificate: %w", back))
		}
		return err
	}
	return nil
}

// stage writes data to a new file beside path, with exactly perm, and returns
// its name.
func stage(path string, data []byte, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".naru-pki-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
