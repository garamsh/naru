// Package inventory is the hosts orch knows: the enrollment keys that let a host ask to join, the
// pending enrollments, the admitted hosts with their region and labels, and the event log.
package inventory

import (
	"errors"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// The failures a caller branches on. Each API transport maps them in one place.
var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflicts with what orch holds")
	ErrInvalid        = errors.New("invalid request")
	ErrKeyRefused     = errors.New("enrollment key refused")
	ErrRateLimited    = errors.New("too many requests")
	ErrCodeMismatch   = errors.New("the pairing code does not match the pending host")
	ErrSignerRefused  = errors.New("the signer refused the admission")
	ErrSignerFailed   = errors.New("the signer failed")
	ErrTooManyPending = errors.New("too many pending enrollments")
)

// Event types.
const (
	EventEnrolling   = "host.enrolling"
	EventAdmitted    = "host.admitted"
	EventLabeled     = "host.labeled"
	EventKeyCreated  = "enrollment-key.created"
	EventKeyRevoked  = "enrollment-key.revoked"
	EventCodeRefused = "host.admission-refused"
)

// Key is an enrollment key as orch stores it: the SHA-256 of the secret, never the secret.
type Key struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Revoked   bool      `json:"revoked"`
}

// Pending is an enrollment waiting for admission.
type Pending struct {
	HostID    string    `json:"hostId"`
	PublicKey []byte    `json:"publicKey"`
	CSR       []byte    `json:"csr"`
	Address   string    `json:"address"`
	KeyID     string    `json:"keyId"`
	FirstSeen time.Time `json:"firstSeen"`
}

// Host is an admitted host.
type Host struct {
	ID          string            `json:"id"`
	PublicKey   []byte            `json:"publicKey"`
	Certificate []byte            `json:"certificate"`
	Region      string            `json:"region"`
	Labels      map[string]string `json:"labels"`
	Address     string            `json:"address"`
	AdmittedAt  time.Time         `json:"admittedAt"`
	LastSeen    time.Time         `json:"lastSeen,omitzero"`
}

func (h Host) api() orchapi.Host {
	labels := h.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return orchapi.Host{ID: h.ID, Region: h.Region, Labels: labels, Address: h.Address, AdmittedAt: h.AdmittedAt, LastSeen: h.LastSeen}
}
