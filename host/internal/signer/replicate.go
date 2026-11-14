package signer

import (
	"bytes"
	"context"
	"crypto/ecdh"
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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

const (
	joinKeyFile    = "join.key"
	joinKeyBlock   = "X25519 PRIVATE KEY"
	joinTimeout    = 30 * time.Second
	maxBundleBytes = 1 << 20
)

// replicateInfo binds a replicated bundle to the host it was authorised for.
func replicateInfo(hostID string) []byte {
	return []byte("naru-signer replicate v1 " + hostID)
}

// Replicate answers a new signer's request for the authorities: for a request root signed for
// its host ID and the ephemeral X25519 key it made, the bundle sealed to that key.
func (s *Signer) Replicate(signed []byte) ([]byte, error) {
	var req identity.ReplicateRequest
	who, a, err := s.authorise(signed, identity.ActionReplicate, &req, identity.RoleRoot)
	if err != nil {
		return nil, err
	}
	if !namePattern.MatchString(req.HostID) {
		return nil, fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}
	pub, err := ecdh.X25519().NewPublicKey(req.X25519)
	if err != nil {
		return nil, fmt.Errorf("%w: x25519 key: %w", ErrBadRequest, err)
	}
	plain, err := a.marshalBundle()
	if err != nil {
		return nil, err
	}
	sealed, err := identity.Seal(pub, plain, replicateInfo(req.HostID))
	if err != nil {
		return nil, err
	}
	s.log.Info("replicated the authorities", "host_id", req.HostID, "authorised_by", who.Subject, "authorised_by_serial", who.Serial)
	return sealed, nil
}

// NewJoinKey makes the ephemeral X25519 key a blank signer asks for the authorities with, keeps
// it in its directory until Join, and returns the public half, which root authorises.
func (s *Signer) NewJoinKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth != nil {
		return nil, ErrInitialised
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate join key: %w", err)
	}
	if err := writeFile(filepath.Join(s.dir, joinKeyFile), pem.EncodeToMemory(&pem.Block{Type: joinKeyBlock, Bytes: key.Bytes()})); err != nil {
		return nil, err
	}
	return key.PublicKey().Bytes(), nil
}

// JoinOptions is what Join asks an existing signer with.
type JoinOptions struct {
	// HostID is the host ID root authorised.
	HostID string
	// PeerURL is the https URL of the existing signer's peer listener.
	PeerURL string
	// PeerCA is the PEM of the CA the peer's serving certificate chains to: the orch authority's.
	PeerCA []byte
	// Authorisation is root's signed ActionReplicate request for HostID and the key NewJoinKey made.
	Authorisation []byte
}

// Join fills a blank signer from an existing one over TLS 1.3: it sends root's authorisation and
// receives the authorities sealed to its join key, which it removes once they are in place.
func (s *Signer) Join(ctx context.Context, opts JoinOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth != nil {
		return ErrInitialised
	}
	priv, err := s.readJoinKey()
	if err != nil {
		return err
	}
	sealed, err := fetchReplica(ctx, opts)
	if err != nil {
		return err
	}
	plain, err := identity.Open(priv, sealed, replicateInfo(opts.HostID))
	if err != nil {
		return err
	}
	a, err := parseBundle(plain)
	if err != nil {
		return err
	}
	who, _, err := identity.VerifyRequest(opts.Authorisation, identity.ActionReplicate, a.operatorPool(), &a.crl, s.now())
	if err != nil {
		return fmt.Errorf("the authorities do not vouch for root's authorisation: %w", err)
	}
	if who.Role != identity.RoleRoot {
		return fmt.Errorf("%w: %s may not authorise a replica", ErrForbidden, who.Role)
	}
	if err := a.save(s.dir); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, joinKeyFile))
	s.auth = a
	s.log.Info("joined: the authorities were replicated from a peer", "host_id", opts.HostID, "peer", opts.PeerURL)
	return nil
}

func (s *Signer) readJoinKey() (*ecdh.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, joinKeyFile)) //nolint:gosec // a file of the signer's own directory
	if err != nil {
		return nil, fmt.Errorf("read the join key (make it with NewJoinKey): %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != joinKeyBlock {
		return nil, fmt.Errorf("%w: %s holds no join key", errCorrupt, joinKeyFile)
	}
	return ecdh.X25519().NewPrivateKey(block.Bytes)
}

// fetchReplica posts root's authorisation to the peer and returns its sealed answer.
func fetchReplica(ctx context.Context, opts JoinOptions) ([]byte, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(opts.PeerCA) {
		return nil, errors.New("the peer CA holds no certificate")
	}
	client := &http.Client{
		Timeout:   joinTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.PeerURL+"/v1/replicate", bytes.NewReader(opts.Authorisation))
	if err != nil {
		return nil, fmt.Errorf("ask the peer: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ask the peer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundleBytes))
	if err != nil {
		return nil, fmt.Errorf("ask the peer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the peer refused: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return body, nil
}

// PeerTLSConfig returns the configuration of the peer listener: TLS 1.3 and a serving certificate
// for names (DNS names or IP addresses) that the orch authority signs now, its key held in memory
// only. A joining signer holds no certificate to present, so the caller is authenticated by
// root's signed request and not by the handshake.
func (s *Signer) PeerTLSConfig(names []string) (*tls.Config, error) {
	a, err := s.ready()
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate serving key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := s.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "naru-signer", OrganizationalUnit: []string{string(identity.RoleOrch)}},
		NotBefore:    now.Add(-notBeforeBackoff),
		NotAfter:     now.AddDate(certificateYears, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.orch.cert, &key.PublicKey, a.orch.key)
	if err != nil {
		return nil, fmt.Errorf("issue the serving certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"http/1.1"},
	}, nil
}
