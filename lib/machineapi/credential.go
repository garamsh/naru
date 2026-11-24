package machineapi

import (
	"context"
	"crypto"
	"crypto/x509"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/machineapi/signedrequest"
)

// RegisterRequest is the body of POST register: a one-time registration token root gave the
// caller, and the certificate request, DER, of a key the caller made where it runs (ADR 0116).
type RegisterRequest struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"`
}

// RenewRequest is the body of POST renew: a request the caller signed with its current
// certificate, for the certificate request of its new key (signedrequest.ActionRenewCaller).
type RenewRequest struct {
	Signed []byte `json:"signed"`
}

// Credential is the answer to register and renew: the caller certificate, DER, and when it
// expires. The key is the caller's own and never crosses the API.
type Credential struct {
	Certificate []byte    `json:"certificate"`
	NotAfter    time.Time `json:"notAfter"`
}

// Register spends token for a caller certificate of the key whose certificate request csr is. It
// presents no client certificate: a Client for it is made with an http.Client that carries none.
func (c *Client) Register(ctx context.Context, token string, csr []byte) (Credential, error) {
	var out Credential
	_, err := c.call(ctx, http.MethodPost, "register", nil, RegisterRequest{Token: token, CSR: csr}, &out)
	return out, err
}

// Renew asks for a further certificate for the key whose certificate request csr is, signed with
// key and cert, the caller's current pair. It is taken in the last 30 days of cert's life; the
// Client presents the caller's certificate as for any other operation.
func (c *Client) Renew(ctx context.Context, key crypto.Signer, cert *x509.Certificate, csr []byte) (Credential, error) {
	signed, err := signedrequest.Sign(signedrequest.ActionRenewCaller, signedrequest.CallerRenewal{CSR: csr}, time.Now().Add(time.Minute), key, cert)
	if err != nil {
		return Credential{}, err
	}
	var out Credential
	_, err = c.call(ctx, http.MethodPost, "renew", nil, RenewRequest{Signed: signed}, &out)
	return out, err
}
