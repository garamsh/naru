// Package enroll is naru-hostd's enrollment with orch: it presents an enrollment key, shows the
// pairing code a person checks, polls for the certificate orch's signer issues once an operator
// admits the host, and then reports to orch with that certificate.
package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// Config is the agent's settings.
type Config struct {
	// OrchURL is orch's https URL and CASHA256 the hex SHA-256 of the authority orch's certificate
	// chains to, which is the only trust the host has before it holds a certificate.
	OrchURL  string
	CASHA256 string
	// EnrollKeyFile holds the enrollment key. It is read by path and never logged.
	EnrollKeyFile string
	// KeyFile is the host's own key, generated 0600 on the first start; the certificate is written
	// beside it, at CertFile.
	KeyFile  string
	CertFile string
	HostID   string
	// Address is where orch and the console can reach this host.
	Address string
	// Out receives the pairing code; stdout in the daemon.
	Out io.Writer
	// CodeEvery, PollEvery and HeartbeatEvery default to 60 s, 5 s and 30 s.
	CodeEvery      time.Duration
	PollEvery      time.Duration
	HeartbeatEvery time.Duration
	Now            func() time.Time
	Log            *slog.Logger
}

// CertPathFor is the certificate path beside a key file: the key's name with .crt.
func CertPathFor(keyFile string) string {
	return strings.TrimSuffix(keyFile, filepath.Ext(keyFile)) + ".crt"
}

// Agent enrolls the host and then keeps reporting to orch.
type Agent struct {
	c Config
}

// New returns an agent with the defaults filled in.
func New(c Config) *Agent {
	if c.CodeEvery == 0 {
		c.CodeEvery = 60 * time.Second
	}
	if c.PollEvery == 0 {
		c.PollEvery = 5 * time.Second
	}
	if c.HeartbeatEvery == 0 {
		c.HeartbeatEvery = 30 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.CertFile == "" {
		c.CertFile = CertPathFor(c.KeyFile)
	}
	return &Agent{c}
}

// Run enrolls when the host holds no certificate for its key, then heartbeats until ctx ends. It
// returns an error when orch refuses the enrollment, which retrying would not change.
func (a *Agent) Run(ctx context.Context) error {
	key, err := loadOrCreateKey(a.c.KeyFile)
	if err != nil {
		return err
	}
	cert, err := a.loadCertificate(key)
	if err != nil {
		return err
	}
	if cert == nil {
		if cert, err = a.enroll(ctx, key); err != nil {
			return err
		}
	}
	return a.heartbeat(ctx, cert)
}

func (a *Agent) client(cert *tls.Certificate) (*orchapi.Client, error) {
	u, err := url.Parse(a.c.OrchURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, fmt.Errorf("orch URL %q is not an https URL", a.c.OrchURL)
	}
	conf, err := orchapi.PinCA(a.c.CASHA256, u.Hostname(), cert)
	if err != nil {
		return nil, err
	}
	return orchapi.New(a.c.OrchURL, orchapi.HTTPClient(conf, 30*time.Second))
}

// loadOrCreateKey reads the host's key, or generates it with mode 0600 when there is none.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an ECDSA key", path)
	}
	return key, nil
}

// loadCertificate returns the certificate beside the key when it is for this key and still valid,
// and nil when the host has to enroll.
func (a *Agent) loadCertificate(key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	b, err := os.ReadFile(a.c.CertFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !key.PublicKey.Equal(leaf.PublicKey) || !a.c.Now().Before(leaf.NotAfter) {
		return nil, nil
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

func (a *Agent) enroll(ctx context.Context, key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	secret, err := os.ReadFile(a.c.EnrollKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the enrollment key: %w", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: a.c.HostID}}, key)
	if err != nil {
		return nil, err
	}
	cl, err := a.client(nil)
	if err != nil {
		return nil, err
	}
	req := orchapi.EnrollRequest{Key: strings.TrimSpace(string(secret)), HostID: a.c.HostID, PublicKey: pub, CSR: csr, Address: a.c.Address}

	var st orchapi.EnrollStatus
	for {
		if st, err = cl.Enroll(ctx, req); err == nil {
			break
		}
		if refused(err) {
			return nil, fmt.Errorf("orch refused the enrollment: %w", err)
		}
		a.c.Log.Warn("enrollment failed, retrying", "err", err.Error())
		if !sleep(ctx, a.c.PollEvery) {
			return nil, ctx.Err()
		}
	}

	lastPrint := time.Time{}
	for st.State != orchapi.StateAdmitted {
		if now := a.c.Now(); lastPrint.IsZero() || now.Sub(lastPrint) >= a.c.CodeEvery {
			_, _ = fmt.Fprintf(a.c.Out, "naru-hostd: host %s waits for admission, pairing code %s\n", a.c.HostID, st.Code)
			lastPrint = now
		}
		if !sleep(ctx, a.c.PollEvery) {
			return nil, ctx.Err()
		}
		next, err := cl.PollEnrollment(ctx, a.c.HostID, key, a.c.Now())
		if err != nil {
			if refused(err) {
				return nil, fmt.Errorf("orch refused the poll: %w", err)
			}
			a.c.Log.Warn("poll failed, retrying", "err", err.Error())
			continue
		}
		if next.Code != "" {
			st.Code = next.Code
		}
		st.State, st.Certificate = next.State, next.Certificate
	}
	if err := writeFileAtomic(a.c.CertFile, st.Certificate, 0o644); err != nil {
		return nil, err
	}
	a.c.Log.Info("admitted", "host", a.c.HostID)
	cert, err := a.loadCertificate(key)
	if err != nil || cert == nil {
		return nil, errors.New("the certificate orch issued is not usable for this host's key")
	}
	return cert, nil
}

// refused reports an answer that retrying does not change: a 4xx other than a rate limit.
func refused(err error) bool {
	var e *orchapi.Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500 && e.Status != 429
}

func (a *Agent) heartbeat(ctx context.Context, cert *tls.Certificate) error {
	cl, err := a.client(cert)
	if err != nil {
		return err
	}
	for {
		if err := cl.Heartbeat(ctx); err != nil && ctx.Err() == nil {
			a.c.Log.Warn("heartbeat failed", "err", err.Error())
		}
		if !sleep(ctx, a.c.HeartbeatEvery) {
			return nil
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("-----BEGIN CERTIFICATE-----")) {
		return errors.New("orch's answer holds no certificate")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cert-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
