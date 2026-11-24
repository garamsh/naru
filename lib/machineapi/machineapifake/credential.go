package machineapifake

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/machineapi/signedrequest"
)

// The caller certificate's life and the window before its end in which it renews, as naru's
// signer has them (ADR 0116).
const (
	callerLife  = 90 * 24 * time.Hour
	renewWindow = 30 * 24 * time.Hour
	callerOU    = "caller"
)

// issuer is the authority the Fake signs callers' certificates with: the Server's. A Fake without
// one answers register and renew 501.
type issuer struct {
	roots *x509.CertPool
	sign  func(name string, pub any, notAfter time.Time) (*x509.Certificate, error)
}

// Token is a registration token for the caller name, taken once, as root asks naru's signer for one.
func (f *Fake) Token(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := "naru-ct-" + rand.Text()
	f.tokens[t] = name
	return t
}

// register spends a token for a caller certificate of 90 days, whatever the outcome.
func (f *Fake) register(r *http.Request) (machineapi.Credential, error) {
	var req machineapi.RegisterRequest
	if err := decode(r, &req); err != nil {
		return machineapi.Credential{}, err
	}
	f.mu.Lock()
	name, ok := f.tokens[req.Token]
	delete(f.tokens, req.Token)
	f.mu.Unlock()
	if f.issuer == nil {
		return machineapi.Credential{}, refuse(http.StatusNotImplemented, machineapi.CodeUnimplemented, "this Fake issues no certificates")
	}
	if !ok {
		return machineapi.Credential{}, refuse(http.StatusForbidden, machineapi.CodeTokenRefused, "the registration token is not valid, or was spent or has expired")
	}
	return f.issueCaller(name, req.CSR)
}

// renew issues a further certificate for a renewal signed in the last 30 days of its certificate,
// for a new key, once per certificate.
func (f *Fake) renew(r *http.Request) (machineapi.Credential, error) {
	var req machineapi.RenewRequest
	if err := decode(r, &req); err != nil {
		return machineapi.Credential{}, err
	}
	if f.issuer == nil {
		return machineapi.Credential{}, refuse(http.StatusNotImplemented, machineapi.CodeUnimplemented, "this Fake issues no certificates")
	}
	chains, raw, err := signedrequest.Open(req.Signed, signedrequest.ActionRenewCaller, f.issuer.roots, time.Now())
	if err != nil || !slices.Equal(chains[0][0].Subject.OrganizationalUnit, []string{callerOU}) {
		return machineapi.Credential{}, refuse(http.StatusForbidden, machineapi.CodeForbidden, "the renewal is not signed by a caller's certificate")
	}
	current := chains[0][0]
	if time.Now().Before(current.NotAfter.Add(-renewWindow)) {
		return machineapi.Credential{}, refuse(http.StatusConflict, machineapi.CodeNotDue, "the certificate is not yet due for renewal")
	}
	var body signedrequest.CallerRenewal
	if err := json.Unmarshal(raw, &body); err != nil {
		return machineapi.Credential{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "the renewal's body: %v", err)
	}
	if csr, err := x509.ParseCertificateRequest(body.CSR); err == nil && sameKey(csr.PublicKey, current.PublicKey) {
		return machineapi.Credential{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "a renewal is for a new key")
	}
	serial := current.SerialNumber.Text(16)
	f.mu.Lock()
	renewed := f.renewed[serial]
	f.renewed[serial] = true
	f.mu.Unlock()
	if renewed {
		return machineapi.Credential{}, refuse(http.StatusForbidden, machineapi.CodeForbidden, "the certificate has renewed already")
	}
	return f.issueCaller(current.Subject.CommonName, body.CSR)
}

func (f *Fake) issueCaller(name string, csrDER []byte) (machineapi.Credential, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return machineapi.Credential{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "the certificate request does not parse")
	}
	cert, err := f.issuer.sign(name, csr.PublicKey, time.Now().Add(callerLife))
	if err != nil {
		return machineapi.Credential{}, err
	}
	return machineapi.Credential{Certificate: cert.Raw, NotAfter: cert.NotAfter}, nil
}

// sameKey reports whether two public keys are one.
func sameKey(a, b any) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}
