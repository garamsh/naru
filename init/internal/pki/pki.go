// Package pki issues the certificates spec.pki asks for, from the authorities
// the configuration carries, and renews them when two thirds of their
// validity has passed (docs/architecture/adr/0013-init-issues-certificates-at-boot.md).
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
func (i *Issuer) Issue(spec config.PKI, addrs []netip.Addr) error {
	i.spec, i.addrs, i.issued = spec, addrs, nil
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
		certPEM, keyPEM, expires, err := i.sign(c)
		if err != nil {
			return fmt.Errorf("issue certificate %s: %w", c.Name, err)
		}
		if err := i.create(c.KeyPath, keyPEM, 0o600); err != nil {
			return fmt.Errorf("write the key of certificate %s: %w", c.Name, err)
		}
		if err := i.create(c.CertificatePath, certPEM, 0o644); err != nil {
			return fmt.Errorf("write certificate %s: %w", c.Name, err)
		}
		i.issued = append(i.issued, &issued{cert: c, due: dueAt(i.clock.Now(), c.ValidFor())})
		i.log.Info("certificate issued", "name", c.Name, "authority", c.Authority, "usage", string(c.Usage), "path", c.CertificatePath, "expires", expires.UTC().Format(time.RFC3339))
	}
	return nil
}

// Run renews each issued certificate when two thirds of its validity has
// passed, until ctx ends. A renewal that fails is logged and retried.
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
	for _, s := range i.issued {
		if ctx.Err() != nil {
			return
		}
		if s.due.After(now) {
			continue
		}
		certPEM, keyPEM, expires, err := i.sign(s.cert)
		if err == nil {
			err = i.replace(s.cert, certPEM, keyPEM)
		}
		if err != nil {
			s.due = now.Add(retryAfter)
			i.log.Error("certificate renewal failed", "name", s.cert.Name, "err", err, "retry", retryAfter.String())
			continue
		}
		s.due = dueAt(now, s.cert.ValidFor())
		i.log.Info("certificate renewed", "name", s.cert.Name, "authority", s.cert.Authority, "path", s.cert.CertificatePath, "expires", expires.UTC().Format(time.RFC3339))
		for _, name := range s.cert.Restart {
			i.restarter.Restart(name)
		}
	}
}

// dueAt is when a certificate issued at now, valid for d, is renewed.
func dueAt(now time.Time, d time.Duration) time.Time {
	return now.Add(d * 2 / 3)
}

// sign returns a new certificate for c with a new key, both in PEM, and when it expires.
func (i *Issuer) sign(c config.Certificate) (certPEM, keyPEM []byte, expires time.Time, err error) {
	auth, ok := i.spec.Authority(c.Authority)
	if !ok {
		return nil, nil, time.Time{}, fmt.Errorf("authority %q is not in spec.pki", c.Authority)
	}
	signer, err := auth.Signer()
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("authority %s: %w", c.Authority, err)
	}
	now := i.clock.Now()
	if now.Before(signer.Certificate.NotBefore) || now.After(signer.Certificate.NotAfter) {
		return nil, nil, time.Time{}, fmt.Errorf("authority %s is not valid at %s", c.Authority, now.UTC().Format(time.RFC3339))
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
	expires = now.Add(c.ValidFor())
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

// replace swaps in a renewed key and certificate, each by rename, the key
// first.
func (i *Issuer) replace(c config.Certificate, certPEM, keyPEM []byte) error {
	if err := i.swap(c.KeyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("replace the key: %w", err)
	}
	if err := i.swap(c.CertificatePath, certPEM, 0o644); err != nil {
		return fmt.Errorf("replace the certificate: %w", err)
	}
	return nil
}

// swap writes data beside path and renames it over path, so a reader sees the
// old file or the new one whole.
func (i *Issuer) swap(path string, data []byte, perm os.FileMode) error {
	p := filepath.Join(i.root, path)
	f, err := os.CreateTemp(filepath.Dir(p), ".naru-pki-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
