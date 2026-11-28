package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// requestLife is how long a renewal or a retire the agent signs stays valid.
const requestLife = 5 * time.Minute

// maintain runs Check now and then every CheckEvery until ctx ends.
func (a *Agent) maintain(ctx context.Context) {
	for {
		if err := a.Check(ctx); err != nil && ctx.Err() == nil {
			a.c.Log.Warn("the daily check of the host's certificate and authorities failed", "err", err.Error())
		}
		if !sleep(ctx, a.c.CheckEvery) {
			return
		}
	}
}

// Check renews the host's certificate when it is due and retires the others, and stores the
// re-signings of the authorities the host trusts (ADR 0122). It does both, and returns what failed.
func (a *Agent) Check(ctx context.Context) error {
	return errors.Join(a.Renew(ctx), a.RefreshTrust(ctx))
}

// retirePath is where a retire not yet done is kept: the serial of the certificate that is to
// sign it, beside the certificate.
func (a *Agent) retirePath() string {
	return strings.TrimSuffix(a.c.CertFile, filepath.Ext(a.c.CertFile)) + ".retire"
}

// Renew renews the certificate the host reports with once it is within identity.HostRenewWindow
// of its end (ADR 0122). It keeps the retire of the host's other certificates first, then signs
// a renewal for its own key, checks that the answer is its own certificate under an authority the
// host trusts, has the host API serve it, reports to orch with it, and signs with it the retire.
// A retire not done is kept and tried again at the next Renew, and a retire that keeps failing does
// not stop the renewal. A certificate that has ended renews nothing: the host is enrolled again
// from a new seed. A rotation (Rotate) waits for a renewal in flight, and a renewal for a rotation.
func (a *Agent) Renew(ctx context.Context) error {
	a.renewMu.Lock()
	defer a.renewMu.Unlock()
	pair := a.currentPair()
	if pair == nil {
		return ErrNotEnrolled
	}
	return errors.Join(a.retirePending(ctx, pair), a.renew(ctx, pair))
}

// renew is Renew's renewal of pair, with renewMu held.
func (a *Agent) renew(ctx context.Context, pair *tls.Certificate) error {
	now := a.c.Now()
	if now.Before(pair.Leaf.NotAfter.Add(-identity.HostRenewWindow)) || !now.Before(pair.Leaf.NotAfter) {
		return nil
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("the host's key is not an ECDSA key")
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: pair.Leaf.Subject.CommonName}}, key)
	if err != nil {
		return err
	}
	signed, err := identity.SignRequest(identity.ActionRenewHost, identity.HostRenewal{CSR: csr}, now.Add(requestLife), key, pair.Leaf)
	if err != nil {
		return err
	}
	issued, err := a.current().RenewHost(ctx, signed)
	var e *orchapi.Error
	if errors.As(err, &e) && e.Status == http.StatusConflict && e.Message == identity.ErrHostNotDue.Error() {
		// The signer's clock is behind the host's: the next check renews.
		a.c.Log.Info("the signer holds the host's certificate not due to renew yet", "not_after", pair.Leaf.NotAfter)
		return nil
	}
	if err != nil {
		return fmt.Errorf("renew the host's certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(issued.Certificate)
	if err != nil {
		return fmt.Errorf("the renewed certificate: %w", err)
	}
	if !key.PublicKey.Equal(leaf.PublicKey) || leaf.Subject.CommonName != pair.Leaf.Subject.CommonName {
		return errors.New("orch answered a certificate that is not the host's own")
	}
	roots, err := a.trusted()
	if err != nil {
		return fmt.Errorf("check the renewed certificate: %w", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("orch answered a certificate under no authority the host trusts: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	// The retire is kept before anything changes, so a crash once the host reports with the new
	// certificate leaves it to the next check; a renewal that stops before then leaves one kept for
	// a serial the host does not hold, which the next check drops.
	if err := writeFileAtomic(a.retirePath(), []byte(identity.SerialOf(leaf)+"\n"), 0o600, ""); err != nil {
		return err
	}
	// The host API serves the new pair first: the retire must not revoke the one it serves.
	if a.c.Serve != nil {
		if err := a.c.Serve(certPEM, keyPEM); err != nil {
			return fmt.Errorf("serve the renewed certificate: %w", err)
		}
	}
	renewed := &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	if err := a.apply(ctx, renewed, certPEM, nil); err != nil {
		return fmt.Errorf("report with the renewed certificate: %w", err)
	}
	a.c.Log.Info("the host renewed its certificate", "serial", identity.SerialOf(leaf), "not_after", leaf.NotAfter)
	return a.retirePending(ctx, renewed)
}

// trusted is the pool of --client-ca and the store of re-signings, which holds the host authority
// a renewed certificate must chain to.
func (a *Agent) trusted() (*x509.CertPool, error) {
	if a.c.ClientCAFile == "" {
		return nil, errors.New("the agent has no pool of authorities to check a certificate against")
	}
	b, err := os.ReadFile(a.c.ClientCAFile)
	if err != nil {
		return nil, err
	}
	if a.c.TrustFile != "" {
		stored, err := os.ReadFile(a.c.TrustFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		b = append(append(b, '\n'), stored...)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no certificate", a.c.ClientCAFile)
	}
	return pool, nil
}

// retirePending signs with pair the retire of the host's other certificates when one is kept for
// pair's serial, and forgets one kept for another, which a rotation since has replaced.
func (a *Agent) retirePending(ctx context.Context, pair *tls.Certificate) error {
	b, err := os.ReadFile(a.retirePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(b)) != identity.SerialOf(pair.Leaf) {
		return os.Remove(a.retirePath())
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("the host's key is not an ECDSA key")
	}
	signed, err := identity.SignRequest(identity.ActionRetireOwnHost, struct{}{}, a.c.Now().Add(requestLife), key, pair.Leaf)
	if err != nil {
		return err
	}
	if err := a.current().RetireHost(ctx, signed); err != nil {
		var e *orchapi.Error
		if errors.As(err, &e) && e.Status == http.StatusConflict {
			// The signer holds no record of the serial, as after a restore from a bundle older
			// than the renewal: no retire it can do will succeed.
			a.c.Log.Warn("the signer does not know the host's certificate, so its retire is dropped", "serial", identity.SerialOf(pair.Leaf), "err", err.Error())
			return os.Remove(a.retirePath())
		}
		return fmt.Errorf("retire the host's other certificates: %w", err)
	}
	a.c.Log.Info("the host retired its other certificates", "kept", identity.SerialOf(pair.Leaf))
	return os.Remove(a.retirePath())
}

// RefreshTrust reads the signer's authorities from orch, over the connection checked by the key
// pin, and stores in TrustFile each that is a re-signing of an authority the host already trusts:
// the same subject and key, ending later than every certificate of it the host holds (ADR 0122).
// A certificate of any other key, under whatever name, is never stored.
func (a *Agent) RefreshTrust(ctx context.Context) error {
	if a.c.TrustFile == "" {
		return nil
	}
	cl := a.current()
	if cl == nil {
		return ErrNotEnrolled
	}
	offered, err := cl.Authorities(ctx)
	if err != nil {
		return fmt.Errorf("read the signer's authorities: %w", err)
	}
	base, err := os.ReadFile(a.c.ClientCAFile)
	if err != nil {
		return err
	}
	stored, err := os.ReadFile(a.c.TrustFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	add, took := identity.Resignings(append(append(append([]byte{}, base...), '\n'), stored...), offered.Host, offered.Operator, offered.Orch)
	for _, c := range took {
		a.c.Log.Info("the host trusts an authority's re-signing", "authority", c.Subject.CommonName, "not_after", c.NotAfter)
	}
	if len(add) == 0 {
		return nil
	}
	return writeFileAtomic(a.c.TrustFile, append(stored, add...), 0o644, "")
}
