// Package enroll is naru-hostd's enrollment with orch: it presents an enrollment key, shows the
// pairing code a person checks, polls for the certificate orch's signer issues once an operator
// admits the host, and then reports to orch with that certificate. A host that booted from the
// install seed (Join) enrolls the same way, under an ID derived from its key and with a report of
// its hardware, receives the configuration orch built for it beside the certificate, writes it for
// the installer and stops.
package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
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
	// Join makes the agent the installer's: it reports Hardware, takes the host's ID from its key (the
	// HostID given is ignored) and its address from the route to orch, writes the configuration orch
	// answers with to JoinConfigFile, and returns instead of reporting to orch.
	Join           bool
	JoinConfigFile string
	// Hardware reports what the host is made of; it is read once, when the host first enrolls.
	Hardware func() (*orchapi.Hardware, error)
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

	// mu guards cl, the client heartbeats go out on: a rotation replaces it.
	mu sync.Mutex
	cl *orchapi.Client
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
	return &Agent{c: c}
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
	if a.c.Join {
		return a.join(ctx, key, cert)
	}
	if cert == nil {
		if cert, _, err = a.enroll(ctx, key); err != nil {
			return err
		}
	}
	return a.heartbeat(ctx, cert)
}

// JoinHostID is the ID a host that booted from the install seed enrolls under: h- and the first 16
// hex digits of the SHA-256 of its public key, PKIX DER. Nothing about the host has to be typed.
func JoinHostID(publicKeyDER []byte) string {
	sum := sha256.Sum256(publicKeyDER)
	return "h-" + hex.EncodeToString(sum[:])[:16]
}

// join enrolls the host and hands the configuration over, unless an earlier run did.
func (a *Agent) join(ctx context.Context, key *ecdsa.PrivateKey, cert *tls.Certificate) error {
	if cert != nil {
		if _, err := os.Stat(a.c.JoinConfigFile); err == nil {
			return nil
		}
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	a.c.HostID = JoinHostID(pub)
	for {
		if a.c.Address, err = AddressToward(a.c.OrchURL); err == nil {
			break
		}
		a.c.Log.Warn("the host has no route to orch yet, retrying", "err", err.Error())
		if !sleep(ctx, a.c.PollEvery) {
			return ctx.Err()
		}
	}
	_, config, err := a.enroll(ctx, key)
	if err != nil {
		return err
	}
	if len(config) == 0 {
		return errors.New("orch admitted the host without a configuration for it")
	}
	return writeFileAtomic(a.c.JoinConfigFile, config, 0o600, "")
}

// AddressToward is the address this host reaches orchURL from, as host:port of the host API: what
// orch lists for a host that has no address of its own to give. It sends nothing.
func AddressToward(orchURL string) (string, error) {
	u, err := url.Parse(orchURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("orch URL %q is not a URL", orchURL)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	c, err := net.Dial("udp4", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("the route to orch has no local address")
	}
	return net.JoinHostPort(ua.IP.String(), wire.HostAPIPort), nil
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

// enroll returns the host's certificate once it is admitted, and the configuration orch answered
// with, which only a host that enrolled from the install seed has.
func (a *Agent) enroll(ctx context.Context, key *ecdsa.PrivateKey) (*tls.Certificate, []byte, error) {
	secret, err := os.ReadFile(a.c.EnrollKeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read the enrollment key: %w", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: a.c.HostID}}, key)
	if err != nil {
		return nil, nil, err
	}
	cl, err := a.client(nil)
	if err != nil {
		return nil, nil, err
	}
	req := orchapi.EnrollRequest{Key: strings.TrimSpace(string(secret)), HostID: a.c.HostID, PublicKey: pub, CSR: csr, Address: a.c.Address}
	if a.c.Hardware != nil {
		if req.Hardware, err = a.c.Hardware(); err != nil {
			return nil, nil, fmt.Errorf("read the hardware: %w", err)
		}
	}

	var st orchapi.EnrollStatus
	for {
		if st, err = cl.Enroll(ctx, req); err == nil {
			break
		}
		if refused(err) {
			return nil, nil, fmt.Errorf("orch refused the enrollment: %w", err)
		}
		a.c.Log.Warn("enrollment failed, retrying", "err", err.Error())
		if !sleep(ctx, a.c.PollEvery) {
			return nil, nil, ctx.Err()
		}
	}

	lastPrint := time.Time{}
	for st.State != orchapi.StateAdmitted {
		if now := a.c.Now(); lastPrint.IsZero() || now.Sub(lastPrint) >= a.c.CodeEvery {
			_, _ = fmt.Fprintf(a.c.Out, "naru-hostd: host %s waits for admission, pairing code %s\n", a.c.HostID, st.Code)
			lastPrint = now
		}
		if !sleep(ctx, a.c.PollEvery) {
			return nil, nil, ctx.Err()
		}
		next, err := cl.PollEnrollment(ctx, a.c.HostID, key, a.c.Now())
		if err != nil {
			if refused(err) {
				return nil, nil, fmt.Errorf("orch refused the poll: %w", err)
			}
			a.c.Log.Warn("poll failed, retrying", "err", err.Error())
			continue
		}
		if next.Code != "" {
			st.Code = next.Code
		}
		st.State, st.Certificate, st.Config = next.State, next.Certificate, next.Config
	}
	if err := writeFileAtomic(a.c.CertFile, st.Certificate, 0o644, certificatePrefix); err != nil {
		return nil, nil, err
	}
	a.c.Log.Info("admitted", "host", a.c.HostID)
	cert, err := a.loadCertificate(key)
	if err != nil || cert == nil {
		return nil, nil, errors.New("the certificate orch issued is not usable for this host's key")
	}
	return cert, st.Config, nil
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
	a.setClient(cl)
	for {
		if err := a.current().Heartbeat(ctx); err != nil && ctx.Err() == nil {
			a.c.Log.Warn("heartbeat failed", "err", err.Error())
		}
		if !sleep(ctx, a.c.HeartbeatEvery) {
			return nil
		}
	}
}

func (a *Agent) current() *orchapi.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cl
}

func (a *Agent) setClient(cl *orchapi.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cl = cl
}

var (
	// ErrNotEnrolled is returned by Rotate on an agent that holds no certificate it reports with yet.
	ErrNotEnrolled = errors.New("the host has not enrolled, so it holds no pair to replace")
	// ErrBadPair is returned by Rotate for a certificate and key that cannot be the host's.
	ErrBadPair = errors.New("not a usable pair for this host")
	// ErrOrchRefused is returned by Rotate when orch does not accept the new pair.
	ErrOrchRefused = errors.New("orch did not accept the new pair")
)

// Rotate replaces the pair the host reports to orch with, from the PEM of a certificate and a PKCS #8
// key (ADR 0077). It checks that they are a pair, that the certificate is this host's (its CN is the
// host ID) and not expired, and has the new pair heartbeat to orch before anything on disk changes:
// an orch that refuses it leaves the old pair in place and in use. It then writes the key and the
// certificate beside it, each atomically, and reports with the new pair from the next heartbeat on.
// It returns the serial of the certificate, in hexadecimal.
func (a *Agent) Rotate(ctx context.Context, certPEM, keyPEM []byte) (string, error) {
	if a.current() == nil {
		return "", ErrNotEnrolled
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadPair, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadPair, err)
	}
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		return "", fmt.Errorf("%w: the key is not an ECDSA key", ErrBadPair)
	}
	if leaf.Subject.CommonName != a.c.HostID {
		return "", fmt.Errorf("%w: the certificate is for %q, not for %q", ErrBadPair, leaf.Subject.CommonName, a.c.HostID)
	}
	if !a.c.Now().Before(leaf.NotAfter) {
		return "", fmt.Errorf("%w: the certificate has expired", ErrBadPair)
	}
	pair.Leaf = leaf
	cl, err := a.client(&pair)
	if err != nil {
		return "", err
	}
	if err := cl.Heartbeat(ctx); err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrchRefused, err)
	}
	if err := writeFileAtomic(a.c.KeyFile, keyPEM, 0o600, ""); err != nil {
		return "", err
	}
	if err := writeFileAtomic(a.c.CertFile, certPEM, 0o644, certificatePrefix); err != nil {
		return "", err
	}
	a.setClient(cl)
	a.c.Log.Info("the host's pair was replaced", "serial", leaf.SerialNumber.Text(16))
	return leaf.SerialNumber.Text(16), nil
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

const certificatePrefix = "-----BEGIN CERTIFICATE-----"

// writeFileAtomic writes b to path through a temporary file beside it and a rename, so a reader
// sees all of it or none. A non-empty prefix is what b must start with.
func writeFileAtomic(path string, b []byte, mode os.FileMode, prefix string) error {
	if prefix != "" && !bytes.HasPrefix(bytes.TrimSpace(b), []byte(prefix)) {
		return errors.New("orch's answer holds no certificate")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(path, b, mode)
}
