package machineapifake

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
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

// renew issues a further certificate for a renewal signed in the last 30 days of its certificate.
func (f *Fake) renew(r *http.Request) (machineapi.Credential, error) {
	var req machineapi.RenewRequest
	if err := decode(r, &req); err != nil {
		return machineapi.Credential{}, err
	}
	if f.issuer == nil {
		return machineapi.Credential{}, refuse(http.StatusNotImplemented, machineapi.CodeUnimplemented, "this Fake issues no certificates")
	}
	who, raw, err := identity.VerifyRequest(req.Signed, identity.ActionRenewCaller, f.issuer.roots, nil, time.Now())
	if err != nil || who.Role != identity.RoleCaller {
		return machineapi.Credential{}, refuse(http.StatusForbidden, machineapi.CodeForbidden, "the renewal is not signed by a caller's certificate")
	}
	if time.Now().Before(who.NotAfter.Add(-identity.CallerRenewWindow)) {
		return machineapi.Credential{}, refuse(http.StatusConflict, machineapi.CodeNotDue, "%v", identity.ErrCallerNotDue)
	}
	var body identity.CallerRenewal
	if err := json.Unmarshal(raw, &body); err != nil {
		return machineapi.Credential{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "the renewal's body: %v", err)
	}
	return f.issueCaller(who.Subject, body.CSR)
}

func (f *Fake) issueCaller(name string, csrDER []byte) (machineapi.Credential, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return machineapi.Credential{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "the certificate request does not parse")
	}
	cert, err := f.issuer.sign(name, csr.PublicKey, time.Now().AddDate(0, 0, identity.MaxCallerDays))
	if err != nil {
		return machineapi.Credential{}, err
	}
	return machineapi.Credential{Certificate: cert.Raw, NotAfter: cert.NotAfter}, nil
}
