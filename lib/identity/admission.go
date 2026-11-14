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
	// ErrAdmissionRole is returned when the signer of a statement is neither an operator nor root.
	ErrAdmissionRole = errors.New("admission statement is not signed by an operator or root")
	// ErrAdmissionExpired is returned when a statement is past its expiry.
	ErrAdmissionExpired = errors.New("admission statement has expired")
	// ErrAdmissionSignature is returned when a statement's signature does not verify.
	ErrAdmissionSignature = errors.New("admission statement signature does not verify")
)

// Admission is what an operator says about a host they let join.
// HostKeySHA256 is the lowercase hexadecimal SHA-256 of the host's public key DER.
type Admission struct {
	HostID        string            `json:"hostID"`
	HostKeySHA256 string            `json:"hostKeySHA256"`
	Region        string            `json:"region"`
	Labels        map[string]string `json:"labels"`
	ExpiresAt     time.Time         `json:"expiresAt"`
}

type statement struct {
	Admission   json.RawMessage `json:"admission"`
	Signature   []byte          `json:"signature"`
	Certificate []byte          `json:"certificate"`
}

// SignAdmission signs a with the operator's ECDSA key and attaches the
// operator's certificate. The signature is over the SHA-256 of the admission JSON.
func SignAdmission(a Admission, key crypto.Signer, cert *x509.Certificate) ([]byte, error) {
	body, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("marshal admission: %w", err)
	}
	sum := sha256.Sum256(body)
	sig, err := key.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign admission: %w", err)
	}
	return json.Marshal(statement{Admission: body, Signature: sig, Certificate: cert.Raw})
}

// VerifyAdmission checks the statement: the attached certificate chains to
// roots, is not on crl, holds the operator or root role, and signed the
// admission, which has not expired at now.
func VerifyAdmission(stmt []byte, roots *x509.CertPool, crl *CRL, now time.Time) (Identity, Admission, error) {
	var s statement
	if err := json.Unmarshal(stmt, &s); err != nil {
		return Identity{}, Admission{}, fmt.Errorf("parse admission statement: %w", err)
	}
	cert, err := x509.ParseCertificate(s.Certificate)
	if err != nil {
		return Identity{}, Admission{}, fmt.Errorf("parse operator certificate: %w", err)
	}
	chains, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return Identity{}, Admission{}, fmt.Errorf("verify operator certificate: %w", err)
	}
	id, err := Authenticate(chains, crl)
	if err != nil {
		return Identity{}, Admission{}, err
	}
	if id.Role != RoleOperator && id.Role != RoleRoot {
		return Identity{}, Admission{}, fmt.Errorf("%w: role %s", ErrAdmissionRole, id.Role)
	}
	if cert.CheckSignature(x509.ECDSAWithSHA256, s.Admission, s.Signature) != nil {
		return Identity{}, Admission{}, ErrAdmissionSignature
	}
	var a Admission
	if err := json.Unmarshal(s.Admission, &a); err != nil {
		return Identity{}, Admission{}, fmt.Errorf("parse admission: %w", err)
	}
	if !now.Before(a.ExpiresAt) {
		return Identity{}, Admission{}, ErrAdmissionExpired
	}
	return id, a, nil
}
