package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/host/internal/api"
	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// maintenanceOptions is what a hostd in maintenance mode reads from its flags (ADR 0082).
type maintenanceOptions struct {
	listen      string
	orchURL     string
	orchCA      string
	hostKey     string
	hostID      string
	reason      string
	resetMarker string
	reboot      func() error
	revocation  revocationConfig
}

// authorityRetry is how long maintenance waits between attempts to reach orch for its authority.
const authorityRetry = 5 * time.Second

// hostIdentity returns the ID a reset must confirm: the flag when it is set, else the common name of
// the certificate enrollment wrote beside the host's key, else the hostname. The enrollment's
// request names the host's ID as its common name.
func hostIdentity(flagID, hostKey string) (string, error) {
	if flagID != "" {
		return flagID, nil
	}
	if certs, err := readChain(enroll.CertPathFor(hostKey)); err == nil && certs[0].Subject.CommonName != "" {
		return certs[0].Subject.CommonName, nil
	}
	return os.Hostname()
}

// readChain reads the PEM certificates of a file: the leaf first.
func readChain(path string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New(path + " holds no certificate")
	}
	return out, nil
}

// pairAfterReset returns the check a reset of the data disk makes first: it is the check that
// chooseServing makes of the configuration's pair, the only one that can serve once the clear has
// deleted the stored pair beside the host's key (serving.pem, on the data disk). It returns
// identity.ErrRevoked when that pair is revoked.
func pairAfterReset(list *identity.RevocationList, certFile string) func(context.Context) error {
	return func(ctx context.Context) error { return checkOwnCertificates(ctx, list, certFile) }
}

// rebootInit asks init, which is process 1, to reboot the host. It is how a reset takes effect: the
// reset is carried out at the next boot.
func rebootInit() error { return syscall.Kill(1, syscall.SIGINT) }

// fetchAuthority returns the pool of orch's chain that the pin names, taken over a TLS handshake
// that orchapi.PinCA verifies: every certificate of the pinned authority's subject and key, which an
// operator's client certificate must chain to (ADR 0082, the key pin of ADR 0122).
func fetchAuthority(ctx context.Context, orchURL, fingerprint string) (*x509.CertPool, error) {
	u, err := url.Parse(orchURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, errors.New("the orch URL is not an https URL")
	}
	conf, err := orchapi.PinCA(fingerprint, u.Hostname(), nil)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: conf}).DialContext(dialCtx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	want, _ := hex.DecodeString(strings.ReplaceAll(strings.ToLower(fingerprint), ":", ""))
	pool, ok := orchapi.PinnedRoots(want, conn.(*tls.Conn).ConnectionState().PeerCertificates[1:])
	if !ok {
		return nil, errors.New("orch's chain holds no authority with the pinned key")
	}
	return pool, nil
}

// storedServingPath is where a rotation stores the serving pair: beside the host's key.
func storedServingPath(hostKey string) string {
	return filepath.Join(filepath.Dir(hostKey), "serving.pem")
}

// servingPair is the pair maintenance serves under, picked by the rule the host API uses at start
// (ADR 0077): the pair a rotation stored beside the host's key, when it is not revoked, else the
// host's enrolled pair beside its key, which chains to the authority the operators' client files
// trust; identity.ErrRevoked when that pair is revoked. A host that never enrolled has none, so it
// serves a self-signed pair that no operator can verify: such a host is not reachable.
func servingPair(ctx context.Context, list *identity.RevocationList, hostKey string, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	serving, err := chooseServing(ctx, list, log, enroll.CertPathFor(hostKey), hostKey, storedServingPath(hostKey))
	if err == nil {
		return serving.GetCertificate, nil
	}
	if errors.Is(err, identity.ErrRevoked) {
		return nil, err
	}
	log.Warn("the host has no enrolled certificate; maintenance serves a self-signed one no operator can verify", "err", err.Error())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naru-hostd maintenance"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	pair := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return pair, nil }, nil
}

// runMaintenance serves the maintenance report and the reset until ctx ends. Its clients are the
// holders of a certificate that chains to the authority orch's chain holds under the fingerprint,
// and that is not on the revocation list; the list is required, so a host that cannot get one
// refuses every client. It waits for orch while it cannot be reached.
func runMaintenance(ctx context.Context, o maintenanceOptions, log *slog.Logger) error {
	if o.orchURL == "" || o.orchCA == "" || o.revocation.keyFile == "" {
		return errors.New("maintenance needs --orch-url, --orch-ca-sha256 and --revocation-public-key")
	}
	id, err := hostIdentity(o.hostID, o.hostKey)
	if err != nil {
		return fmt.Errorf("the host's ID: %w", err)
	}
	revocations, err := newRevocations(o.revocation, log)
	if err != nil {
		return err
	}
	pair, err := servingPair(ctx, revocations, o.hostKey, log)
	if err != nil {
		return err
	}
	var pool *x509.CertPool
	for {
		if pool, err = fetchAuthority(ctx, o.orchURL, o.orchCA); err == nil {
			break
		}
		log.Warn("maintenance cannot reach orch for its authority; retrying", "err", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(authorityRetry):
		}
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	go prefetch(ctx, revocations, log)
	srv := api.New(api.Deps{
		Log: log, Metrics: prometheus.NewRegistry(), Revocations: revocations,
		HostID: id, ResetMarker: o.resetMarker, Reboot: o.reboot,
	})
	log.Info("naru-hostd in maintenance mode", "addr", o.listen, "host", id, "reason", o.reason)
	return srv.ServeHandler(ctx, ln, api.ServerTLS(pair, pool, revocations), srv.MaintenanceHandler(api.MaintenanceInfo{HostID: id, Reason: o.reason}))
}
