package identity

import (
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/machineapi/signedrequest"
)

// The form of a signed request is lib/machineapi/signedrequest's, which a Machine API caller
// signs its renewal with and which needs no module of naru.
var (
	// ErrRequestAction is returned when a request was signed for another action.
	ErrRequestAction = signedrequest.ErrAction
	// ErrRequestExpired is returned when a request is past its expiry.
	ErrRequestExpired = signedrequest.ErrExpired
	// ErrRequestSignature is returned when a request's signature does not verify.
	ErrRequestSignature = signedrequest.ErrSignature
)

// The actions a signer's requests are signed for.
const (
	ActionIssueOrch     = "issue-orch"
	ActionIssueOperator = "issue-operator"
	ActionIssueCaller   = "issue-caller"
	// ActionIssueCallerToken asks for a caller registration token.
	ActionIssueCallerToken = "issue-caller-token"
	// ActionRenewCaller is signed by a caller's current certificate to renew it.
	ActionRenewCaller = signedrequest.ActionRenewCaller
	// ActionRevokeCaller revokes every certificate of a caller by its name.
	ActionRevokeCaller = "revoke-caller"
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

// CallerTokenPrefix begins every caller registration token: the signer's base64url claims and
// signature follow it, separated by a dot.
const CallerTokenPrefix = "naru-ct-"

// CallerTokenNonce is the nonce a registration token claims, read without verifying the token, for
// a log line that ties a refused or limited registration to the token the signer issued: "" when
// the token does not read as one, or names a nonce longer than any the signer issues.
func CallerTokenNonce(token string) string {
	body, ok := strings.CutPrefix(token, CallerTokenPrefix)
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(body, ".")
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return ""
	}
	var c struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(payload, &c) != nil || len(c.Nonce) > 128 {
		return ""
	}
	return c.Nonce
}

// CallerRegistration is the body of the signer's POST /v1/caller-register: a registration token, which is
// the only authentication, and the certificate request of a key the caller made, DER.
type CallerRegistration struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"`
}

// CallerRenewal is the body of ActionRenewCaller: the certificate request of the caller's new
// key, DER.
type CallerRenewal = signedrequest.CallerRenewal

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

// CallerRevokeRequest is the body of ActionRevokeCaller: the caller to revoke.
type CallerRevokeRequest struct {
	Name string `json:"name"`
}

// CallerSerial is a serial the signer issued to a caller, in lowercase hexadecimal: the serial of
// the certificate whose renewal it was (Parent, empty for a registration or root's issuance), and
// whether the revocation list carries it.
type CallerSerial struct {
	Serial  string `json:"serial"`
	Parent  string `json:"parent,omitempty"`
	Revoked bool   `json:"revoked"`
}

// CallerRevocation is the answer to ActionRevokeCaller: the revocation list's entry for the caller,
// RevokedAt, before which (as a certificate's NotBefore, inclusive) every caller certificate of the
// name is revoked whichever signer issued it, and the serials this signer issued to it.
type CallerRevocation struct {
	Name      string         `json:"name"`
	RevokedAt time.Time      `json:"revokedAt"`
	Serials   []CallerSerial `json:"serials"`
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

// SignRequest signs body for action with the caller's ECDSA key and attaches the caller's
// certificate. The signature is over the request JSON, which binds the action, the expiry and
// the body.
func SignRequest(action string, body any, expiresAt time.Time, key crypto.Signer, cert *x509.Certificate) ([]byte, error) {
	return signedrequest.Sign(action, body, expiresAt, key, cert)
}

// ReadRequest returns the action and the body of a signed request without verifying it, for orch,
// which forwards an operator's request to the signer that verifies it.
func ReadRequest(data []byte) (action string, body json.RawMessage, err error) {
	return signedrequest.Read(data)
}

// VerifyRequest checks that the attached certificate chains to roots for client authentication,
// is not on crl, and signed a request for action that has not expired at now. It returns the
// signer's Identity and the body, which the caller decodes and whose role it checks.
func VerifyRequest(data []byte, action string, roots *x509.CertPool, crl *CRL, now time.Time) (Identity, json.RawMessage, error) {
	chains, body, err := signedrequest.Open(data, action, roots, now)
	if err != nil {
		return Identity{}, nil, err
	}
	id, err := Authenticate(chains, crl)
	if err != nil {
		return Identity{}, nil, err
	}
	return id, body, nil
}
