// Package renewal keeps an orch member's certificate and its trust in the signer's authorities
// current while it runs (ADR 0123).
//
// init writes the member's first pair (--cert) from the stored configuration at every boot, so a
// certificate renewed while orch runs is kept apart from it, on the data disk (--renewed-cert),
// and every reader of the member's pair reads that file: the API, the Machine API, etcd, orch's
// own client of etcd and its client of the hosts. Prepare chooses that file's content at start;
// a Renewer renews it in its last identity.OrchRenewWindow, retires the member's other
// certificates with it, and keeps the re-signings of the authorities the member trusts in the
// store (--ca-store) every pool reads beside its own file.
package renewal

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/signer"
)

// DefaultCheckEvery is how often a Renewer checks when its Config names no interval.
const DefaultCheckEvery = 24 * time.Hour

// requestLife is how long a renewal or a retire the member signs stays valid.
const requestLife = 5 * time.Minute

// Signer is what a Renewer needs of the local signer.
type Signer interface {
	RenewOrch(ctx context.Context, signed []byte) (identity.Issued, error)
	RetireOrch(ctx context.Context, signed []byte) error
	Authorities(ctx context.Context) (identity.Authorities, error)
}

// Revocations is the check of a certificate against the signer's revocation list.
type Revocations interface {
	Check(ctx context.Context, chains [][]*x509.Certificate) error
}

// Config names the member's files and what a Renewer calls.
type Config struct {
	// FirstCert is --cert, the pair init writes at each boot: the member's certificate followed by
	// its authority's. Key is --key, the member's key, which a renewal keeps.
	FirstCert, Key string
	// CertFile is --renewed-cert, the pair every reader reads: FirstCert's content, or the
	// certificate renewed since and its authority's.
	CertFile string
	// CA is --ca, HostCA --host-ca (empty when it is CA), and Store --ca-store, the re-signings of
	// their authorities the member has taken since.
	CA, HostCA, Store string
	// EtcdCA is the file etcd trusts: CA and Store as they stood at start, since etcd reads it once.
	EtcdCA      string
	Signer      Signer
	Revocations Revocations
	CheckEvery  time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

func (c Config) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// Prepare writes CertFile and EtcdCA before anything reads them. CertFile keeps the certificate it
// holds when that is a pair with Key, chains to an authority of CA and Store, has not ended, is not
// revoked and ends after FirstCert's; otherwise, as at the first start, after a reset of the data
// disk or once an operator issued the member a new pair, it is written from FirstCert. A list that
// cannot be had does not stop a certificate that is otherwise good.
func Prepare(ctx context.Context, c Config) error {
	if c.CertFile == c.FirstCert {
		return errors.New("the renewed certificate's file is the first pair's: init would overwrite it at each boot")
	}
	for _, dir := range []string{filepath.Dir(c.CertFile), filepath.Dir(c.Store), filepath.Dir(c.EtcdCA)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	first, err := os.ReadFile(c.FirstCert)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(c.Key)
	if err != nil {
		return err
	}
	firstPair, err := tls.X509KeyPair(first, key)
	if err != nil {
		return fmt.Errorf("the first pair: %w", err)
	}
	if why := c.keep(ctx, firstPair.Leaf, key); why != "" {
		c.log().Info("the member presents its first certificate", "why", why, "serial", identity.SerialOf(firstPair.Leaf))
		if err := atomicfile.Write(c.CertFile, first, 0o600); err != nil {
			return err
		}
	}
	etcdCA, err := identity.FilesPEM(c.CA, c.Store)
	if err != nil {
		return err
	}
	return atomicfile.Write(c.EtcdCA, etcdCA, 0o644)
}

// keep is why CertFile is not kept over first, or "" when it is. key is the PEM of Key.
func (c Config) keep(ctx context.Context, first *x509.Certificate, key []byte) string {
	b, err := os.ReadFile(c.CertFile)
	if errors.Is(err, os.ErrNotExist) {
		return "no renewed certificate"
	}
	if err != nil {
		return err.Error()
	}
	pair, err := tls.X509KeyPair(b, key)
	if err != nil {
		return "the renewed certificate is not a pair with the key: " + err.Error()
	}
	leaf := pair.Leaf
	if !leaf.NotAfter.After(first.NotAfter) {
		return "the first certificate ends no earlier than the renewed one"
	}
	chains, err := c.verify(leaf, c.now())
	if err != nil {
		return "the renewed certificate: " + err.Error()
	}
	if c.Revocations != nil {
		if err := c.Revocations.Check(ctx, chains); errors.Is(err, identity.ErrRevoked) {
			return "the renewed certificate is revoked"
		} else if err != nil {
			c.log().Warn("the revocation list cannot be had: the renewed certificate is kept unchecked", "err", err.Error())
		}
	}
	return ""
}

// verify is leaf's chains to an authority of CA and Store, for both the uses a member's
// certificate is put to.
func (c Config) verify(leaf *x509.Certificate, now time.Time) ([][]*x509.Certificate, error) {
	b, err := identity.FilesPEM(c.CA, c.Store)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no certificate", c.CA)
	}
	var chains [][]*x509.Certificate
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if chains, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return nil, err
		}
	}
	return chains, nil
}

// Renewer is the member's check of its certificate and its authorities.
type Renewer struct {
	c Config
}

// New is the Renewer of c.
func New(c Config) *Renewer {
	if c.CheckEvery <= 0 {
		c.CheckEvery = DefaultCheckEvery
	}
	return &Renewer{c: c}
}

// Run checks now and then every CheckEvery until ctx ends.
func (r *Renewer) Run(ctx context.Context) {
	t := time.NewTicker(r.c.CheckEvery)
	defer t.Stop()
	for {
		if err := r.Check(ctx); err != nil && ctx.Err() == nil {
			r.c.log().Warn("the daily check of the member's certificate and authorities failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check stores the re-signings of the member's authorities, then renews its certificate when it
// is due and retires its others. It does both, and returns what failed.
func (r *Renewer) Check(ctx context.Context) error {
	return errors.Join(r.RefreshTrust(ctx), r.Renew(ctx))
}

// retirePath is where a retire not yet done is kept: the serial of the certificate that is to
// sign it, beside the certificate.
func (r *Renewer) retirePath() string {
	return strings.TrimSuffix(r.c.CertFile, filepath.Ext(r.c.CertFile)) + ".retire"
}

// Renew renews the certificate in CertFile once it is within identity.OrchRenewWindow of its end.
// It keeps the retire of the member's other certificates first, then signs a renewal for its own
// key, checks that the answer is its own certificate under an authority it trusts, writes it to
// CertFile with that authority's certificate, which every reader presents from its next handshake
// on, and signs with it the retire. A retire not done is kept and tried again at the next Renew,
// and a retire that keeps failing does not stop the renewal. A certificate that has ended renews
// nothing: root issues the member a new one.
func (r *Renewer) Renew(ctx context.Context) error {
	pair, err := tls.LoadX509KeyPair(r.c.CertFile, r.c.Key)
	if err != nil {
		return fmt.Errorf("read the member's pair: %w", err)
	}
	return errors.Join(r.retirePending(ctx, &pair), r.renew(ctx, &pair))
}

// renew is Renew's renewal of pair.
func (r *Renewer) renew(ctx context.Context, pair *tls.Certificate) error {
	now := r.c.now()
	if now.Before(pair.Leaf.NotAfter.Add(-identity.OrchRenewWindow)) || !now.Before(pair.Leaf.NotAfter) {
		return nil
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return errors.New("the member's key does not sign")
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: pair.Leaf.Subject.CommonName}}, key)
	if err != nil {
		return err
	}
	signed, err := identity.SignRequest(identity.ActionRenewOrch, identity.OrchRenewal{CSR: csr}, now.Add(requestLife), key, pair.Leaf)
	if err != nil {
		return err
	}
	issued, err := r.c.Signer.RenewOrch(ctx, signed)
	if errors.Is(err, identity.ErrOrchNotDue) {
		// The signer's clock is behind the member's: the next check renews.
		r.c.log().Info("the signer holds the member's certificate not due to renew yet", "not_after", pair.Leaf.NotAfter)
		return nil
	}
	if err != nil {
		return fmt.Errorf("renew the member's certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(issued.Certificate)
	if err != nil {
		return fmt.Errorf("the renewed certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) || leaf.Subject.CommonName != pair.Leaf.Subject.CommonName {
		return errors.New("the signer answered a certificate that is not the member's own")
	}
	if _, err := r.c.verify(leaf, now); err != nil {
		return fmt.Errorf("the signer answered a certificate under no authority the member trusts: %w", err)
	}
	authority, err := r.c.authorityOf(leaf, now)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.Raw})...)
	// The retire is kept before anything changes, so a crash once the member presents the new
	// certificate leaves it to the next check; a renewal that stops before then leaves one kept
	// for a serial the member does not hold, which the next check drops.
	if err := atomicfile.Write(r.retirePath(), []byte(identity.SerialOf(leaf)+"\n"), 0o600); err != nil {
		return err
	}
	if err := atomicfile.Write(r.c.CertFile, certPEM, 0o600); err != nil {
		return fmt.Errorf("write the renewed certificate: %w", err)
	}
	r.c.log().Info("the member renewed its certificate", "serial", identity.SerialOf(leaf), "not_after", leaf.NotAfter)
	return r.retirePending(ctx, &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf})
}

// authorityOf is the certificate of CA and Store that signed leaf and ends last of those in force
// at now: the authority's latest re-signing, which a host pinning the authority's key takes as its
// root.
func (c Config) authorityOf(leaf *x509.Certificate, now time.Time) (*x509.Certificate, error) {
	b, err := identity.FilesPEM(c.CA, c.Store)
	if err != nil {
		return nil, err
	}
	var latest *x509.Certificate
	for _, a := range identity.Certificates(b) {
		if !a.IsCA || !bytes.Equal(a.RawSubject, leaf.RawIssuer) || now.Before(a.NotBefore) || now.After(a.NotAfter) || leaf.CheckSignatureFrom(a) != nil {
			continue
		}
		if latest == nil || a.NotAfter.After(latest.NotAfter) {
			latest = a
		}
	}
	if latest == nil {
		return nil, errors.New("no authority the member trusts signed the renewed certificate")
	}
	return latest, nil
}

// retirePending signs with pair the retire of the member's other certificates when one is kept
// for pair's serial, and forgets one kept for another.
func (r *Renewer) retirePending(ctx context.Context, pair *tls.Certificate) error {
	b, err := os.ReadFile(r.retirePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(b)) != identity.SerialOf(pair.Leaf) {
		return os.Remove(r.retirePath())
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return errors.New("the member's key does not sign")
	}
	signed, err := identity.SignRequest(identity.ActionRetireOwnOrch, struct{}{}, r.c.now().Add(requestLife), key, pair.Leaf)
	if err != nil {
		return err
	}
	if err := r.c.Signer.RetireOrch(ctx, signed); err != nil {
		if errors.Is(err, signer.ErrUnknownHost) {
			// The signer holds no record of the serial, as after a restore from a bundle older
			// than the renewal: no retire it can do will succeed.
			r.c.log().Warn("the signer does not know the member's certificate, so its retire is dropped", "serial", identity.SerialOf(pair.Leaf), "err", err.Error())
			return os.Remove(r.retirePath())
		}
		return fmt.Errorf("retire the member's other certificates: %w", err)
	}
	r.c.log().Info("the member retired its other certificates", "kept", identity.SerialOf(pair.Leaf))
	return os.Remove(r.retirePath())
}

// RefreshTrust reads the signer's authorities and stores in Store each that is a re-signing of an
// authority CA, HostCA or Store already holds: the same subject and key, ending later than every
// certificate of it held (identity.Resignings). A certificate of any other key is never stored.
func (r *Renewer) RefreshTrust(ctx context.Context) error {
	offered, err := r.c.Signer.Authorities(ctx)
	if err != nil {
		return fmt.Errorf("read the signer's authorities: %w", err)
	}
	trusted, err := identity.FilesPEM(r.c.CA, r.c.HostCA, r.c.Store)
	if err != nil {
		return err
	}
	add, took := identity.Resignings(trusted, offered.Host, offered.Operator, offered.Orch)
	for _, c := range took {
		r.c.log().Info("the member trusts an authority's re-signing", "authority", c.Subject.CommonName, "not_after", c.NotAfter)
	}
	if len(add) == 0 {
		return nil
	}
	stored, err := os.ReadFile(r.c.Store)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicfile.Write(r.c.Store, append(stored, add...), 0o644)
}
