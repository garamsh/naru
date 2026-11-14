// Package orchapi is the contract of naru-orchd's HTTPS API: the request and response types, the
// proof an enrolling host signs, and a Go client for naructl, the console and naru-hostd.
package orchapi

import (
	"fmt"
	"time"
)

// The states an enrolling host is in.
const (
	StatePending  = "pending"
	StateAdmitted = "admitted"
)

// Host is an admitted host.
type Host struct {
	ID         string            `json:"id"`
	Region     string            `json:"region"`
	Labels     map[string]string `json:"labels"`
	Address    string            `json:"address"`
	AdmittedAt time.Time         `json:"admittedAt"`
	LastSeen   time.Time         `json:"lastSeen,omitzero"`
	// Draining is whether an operator drained the host: its members move off it and nothing is
	// placed on it until it is undrained.
	Draining bool `json:"draining"`
}

// Pending is an enrollment waiting for a person's admission. Code is orch's own computation from
// the stored key.
type Pending struct {
	HostID string `json:"hostId"`
	Code   string `json:"code"`
	// PublicKey is the PKIX DER of the host's key, so an operator's tool computes the pairing code
	// and the key hash it signs itself and does not take orch's word for either.
	PublicKey []byte    `json:"publicKey"`
	Address   string    `json:"address"`
	KeyID     string    `json:"keyId"`
	FirstSeen time.Time `json:"firstSeen"`
}

// EnrollRequest is what a host sends with the enrollment key. PublicKey is the PKIX DER of the
// host's own key and CSR the DER certificate request for the same key.
type EnrollRequest struct {
	Key       string `json:"key"`
	HostID    string `json:"hostId"`
	PublicKey []byte `json:"publicKey"`
	CSR       []byte `json:"csr"`
	Address   string `json:"address"`
}

// EnrollStatus answers an enrollment and each poll of it. Certificate, the PEM of the host's
// certificate, is set once State is admitted.
type EnrollStatus struct {
	State       string `json:"state"`
	Code        string `json:"code,omitempty"`
	Certificate []byte `json:"certificate,omitempty"`
}

// AdmitRequest is an operator's admission of one pending host. Code is what the operator read on
// the host's console; Statement is the operator-signed admission statement, which orch passes to
// the signer without reading; Region and Labels are the attributes the statement carries.
type AdmitRequest struct {
	HostID    string            `json:"hostId"`
	Code      string            `json:"code"`
	Statement []byte            `json:"statement"`
	Region    string            `json:"region"`
	Labels    map[string]string `json:"labels"`
}

// CreateKeyRequest asks for an enrollment key that expires at ExpiresAt.
type CreateKeyRequest struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// EnrollmentKey is a created key. Secret is returned once and never stored by orch.
type EnrollmentKey struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SetLabelsRequest replaces a host's region and labels.
type SetLabelsRequest struct {
	Region string            `json:"region"`
	Labels map[string]string `json:"labels"`
}

// Event is one entry of the append-only event log.
type Event struct {
	Time    time.Time         `json:"time"`
	Type    string            `json:"type"`
	Subject string            `json:"subject"`
	Detail  map[string]string `json:"detail,omitempty"`
}

// SnapshotResult names the sealed snapshot orch wrote.
type SnapshotResult struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// Error is an error answer. Every non-2xx response carries it as {"error": "..."}.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return fmt.Sprintf("orch answered %d: %s", e.Status, e.Message) }

// SnapshotInfo binds a sealed snapshot to its purpose: lib/identity's Open fails under any other.
const SnapshotInfo = "naru orch snapshot v1"

// HeaderSnapshotSHA256 carries the hex SHA-256 of the plain snapshot a restore sends.
const HeaderSnapshotSHA256 = "X-Naru-Snapshot-Sha256"
