package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrRequestAction is returned when a request was signed for another action.
	ErrRequestAction = errors.New("request was signed for another action")
	// ErrRequestExpired is returned when a request is past its expiry.
	ErrRequestExpired = errors.New("request has expired")
	// ErrRequestSignature is returned when a request's signature does not verify.
	ErrRequestSignature = errors.New("request signature does not verify")
)

// The actions a signer's requests are signed for.
const (
	ActionIssueOrch     = "issue-orch"
	ActionIssueOperator = "issue-operator"
	ActionIssueCaller   = "issue-caller"
	// ActionIssueCallerToken asks for a caller registration token.
	ActionIssueCallerToken = "issue-caller-token"
	// ActionRenewCaller is signed by a caller's current certificate to renew it.
	ActionRenewCaller  = "renew-caller"
	ActionRevoke       = "revoke"
	ActionReissueHost  = "reissue-host"
	ActionRegisterHost = "register-host"
	ActionRetireHost   = "retire-host"
	ActionLookupHost   = "lookup-host"
	ActionRevokeHost   = "revoke-host"
	ActionBundle       = "bundle"
	ActionRestore      = "restore"
	ActionReplicate    = "replicate"
)

// HostRequest is the body of the signer's POST /v1/host: an admission statement and the
// host's PKCS #10 certificate request, DER.
type HostRequest struct {
	Statement []byte `json:"statement"`
	CSR       []byte `json:"csr"`
}

// IssueRequest is the body of ActionIssueOrch and ActionIssueOperator: the name the
// certificate takes as its CN, the role (operator, reader or automation; none for orch) and the
// certificate request, DER.
type IssueRequest struct {
	Name string `json:"name"`
	Role Role   `json:"role,omitempty"`
	CSR  []byte `json:"csr"`
}

// CallerRequest is the body of ActionIssueCaller: the caller's name, the days the certificate is
// valid (MaxCallerDays when 0) and the certificate request, DER. The signer takes the CN from the
// name; the request names no other.
type CallerRequest struct {
	Name string `json:"name"`
	Days int    `json:"days,omitempty"`
	CSR  []byte `json:"csr"`
}

// MaxCallerDays is the longest a caller certificate is valid, and its default.
const MaxCallerDays = 90

// CallerTokenRequest is the body of ActionIssueCallerToken: the caller the token registers and the
// seconds it lives (DefaultCallerTokenTTL when 0, at most MaxCallerTokenTTL).
type CallerTokenRequest struct {
	Name       string `json:"name"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
}

// CallerToken is the signer's answer to ActionIssueCallerToken: the token, which the signer keeps
// no copy of, and when it expires.
type CallerToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// CallerRegistration is the body of the signer's POST /v1/caller: a registration token, which is
// the only authentication, and the certificate request of a key the caller made, DER.
type CallerRegistration struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"`
}

// CallerRenewal is the body of ActionRenewCaller: the certificate request of the caller's new
// key, DER.
type CallerRenewal struct {
	CSR []byte `json:"csr"`
}

const (
	// DefaultCallerTokenTTL is how long a caller registration token lives when none is asked.
	DefaultCallerTokenTTL = time.Hour
	// MaxCallerTokenTTL is the longest a caller registration token lives.
	MaxCallerTokenTTL = 24 * time.Hour
	// CallerRenewWindow is the end of a caller certificate's life in which it may renew itself.
	CallerRenewWindow = 30 * 24 * time.Hour
)

// ErrCallerNotDue is the refusal of a caller's renewal before the last CallerRenewWindow of its
// certificate's life. The signer answers it 409 with this text, which orch reads back.
var ErrCallerNotDue = errors.New("the certificate is not yet due for renewal")

// RevokeRequest is the body of ActionRevoke.
type RevokeRequest struct {
	Serial string `json:"serial"`
}

// ReissueHostRequest is the body of ActionReissueHost: the host that is given a further certificate,
// for the key in the certificate request, DER. The certificates it already holds stay valid.
type ReissueHostRequest struct {
	HostID string `json:"hostID"`
	CSR    []byte `json:"csr"`
}

// RetireHostRequest is the body of ActionRetireHost: the host and the serial, in hexadecimal, of the
// certificate it keeps. The signer revokes every other serial it holds for the host.
type RetireHostRequest struct {
	HostID string `json:"hostID"`
	Keep   string `json:"keep"`
}

// RegisterHostRequest is the body of ActionRegisterHost: a host and the serial, in hexadecimal, of
// a certificate it holds that another authority issued, which the signer will then revoke on rotation.
type RegisterHostRequest struct {
	HostID string `json:"hostID"`
	Serial string `json:"serial"`
}

// HostSerialsRequest is the body of ActionLookupHost and ActionRevokeHost: the host whose serials the
// signer lists, or revokes whole.
type HostSerialsRequest struct {
	HostID string `json:"hostID"`
}

// HostSerial is a serial the signer holds for a host, in lowercase hexadecimal, and whether the
// revocation list carries it.
type HostSerial struct {
	Serial  string `json:"serial"`
	Revoked bool   `json:"revoked"`
}

// HostSerials is the answer to ActionLookupHost and ActionRevokeHost: every serial the signer holds for
// the host, as the revocation list stands once the call is done.
type HostSerials struct {
	HostID  string       `json:"hostID"`
	Serials []HostSerial `json:"serials"`
}

// RestoreRequest is the body of ActionRestore: the lowercase hexadecimal SHA-256 of the sealed
// bundle root authorises.
type RestoreRequest struct {
	BundleSHA256 string `json:"bundleSHA256"`
}

// RestoreBody is the body of the signer's POST /v1/restore: the sealed bundle, root's signed
// ActionRestore request that names it, and the paths on the signer's host of the recovery key
// file and of the file holding its passphrase.
type RestoreBody struct {
	Bundle         []byte `json:"bundle"`
	Authorisation  []byte `json:"authorisation"`
	RecoveryKey    string `json:"recoveryKey"`
	PassphraseFile string `json:"passphraseFile"`
}

// Issued is a certificate a signer issued: DER, its serial in lowercase hexadecimal, and its expiry.
type Issued struct {
	Certificate []byte    `json:"certificate"`
	Serial      string    `json:"serial"`
	NotAfter    time.Time `json:"notAfter"`
}

// Authorities is what a signer answers to GET /v1/ca: the certificates of its three authorities,
// DER, and the public half of the revocation key.
type Authorities struct {
	Host          []byte `json:"host"`
	Operator      []byte `json:"operator"`
	Orch          []byte `json:"orch"`
	RevocationKey []byte `json:"revocationKey"`
}

// ReplicateRequest is the body of ActionReplicate: root's authorisation for the signer of HostID
// to receive the bundle, sealed to X25519, the ephemeral public key the new signer made.
type ReplicateRequest struct {
	HostID string `json:"hostID"`
	X25519 []byte `json:"x25519"`
}

type request struct {
	Action    string          `json:"action"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Body      json.RawMessage `json:"body"`
}

type signedRequest struct {
	Request     json.RawMessage `json:"request"`
	Signature   []byte          `json:"signature"`
	Certificate []byte          `json:"certificate"`
}

// SignRequest signs body for action with the caller's ECDSA key and attaches the caller's
// certificate. The signature is over the request JSON, which binds the action, the expiry and
// the body.
func SignRequest(action string, body any, expiresAt time.Time, key crypto.Signer, cert *x509.Certificate) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := json.Marshal(request{Action: action, ExpiresAt: expiresAt, Body: raw})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	sum := sha256.Sum256(req)
	sig, err := key.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}
	return json.Marshal(signedRequest{Request: req, Signature: sig, Certificate: cert.Raw})
}

// VerifyRequest checks that the attached certificate chains to roots for client authentication,
// is not on crl, and signed a request for action that has not expired at now. It returns the
// signer's Identity and the body, which the caller decodes and whose role it checks.
func VerifyRequest(data []byte, action string, roots *x509.CertPool, crl *CRL, now time.Time) (Identity, json.RawMessage, error) {
	var s signedRequest
	if err := json.Unmarshal(data, &s); err != nil {
		return Identity{}, nil, fmt.Errorf("parse signed request: %w", err)
	}
	cert, err := x509.ParseCertificate(s.Certificate)
	if err != nil {
		return Identity{}, nil, fmt.Errorf("parse request certificate: %w", err)
	}
	chains, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return Identity{}, nil, fmt.Errorf("verify request certificate: %w", err)
	}
	id, err := Authenticate(chains, crl)
	if err != nil {
		return Identity{}, nil, err
	}
	if cert.CheckSignature(x509.ECDSAWithSHA256, s.Request, s.Signature) != nil {
		return Identity{}, nil, ErrRequestSignature
	}
	var req request
	if err := json.Unmarshal(s.Request, &req); err != nil {
		return Identity{}, nil, fmt.Errorf("parse request: %w", err)
	}
	if req.Action != action {
		return Identity{}, nil, fmt.Errorf("%w: %q, not %q", ErrRequestAction, req.Action, action)
	}
	if !now.Before(req.ExpiresAt) {
		return Identity{}, nil, ErrRequestExpired
	}
	return id, req.Body, nil
}
