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
	// ErrPoolFull is an admission that finds every prefix of the Machine pool given to a host.
	ErrPoolFull = errors.New("the Machine pool has no free prefix")
	// ErrRedacted is a profile set with the redaction GET /v1/profile shows in place of a secret.
	ErrRedacted = errors.New("the host profile holds a redacted secret")
	// ErrNoProfile is the admission of a host that joined from the install seed while no host profile
	// is set, so there is nothing to build its configuration from.
	ErrNoProfile = errors.New("no host profile is set")
	// ErrAllocationTaken is the store's answer to an admission whose prefix another admission took first.
	ErrAllocationTaken = errors.New("the prefix was allocated meanwhile")
	// ErrPrefixUnrecorded is the admission of a joined host while an admitted host has no Machine prefix
	// recorded: the pool must not give out a prefix that host holds.
	ErrPrefixUnrecorded = errors.New("an admitted host's Machine prefix is not recorded")
	// ErrAlreadyAdmitted is the store's answer to an enrollment write that finds the host admitted.
	ErrAlreadyAdmitted = errors.New("the host is admitted")
)

// Event types.
const (
	EventEnrolling   = "host.enrolling"
	EventAdmitted    = "host.admitted"
	EventLabeled     = "host.labeled"
	EventDrained     = "host.drained"
	EventUndrained   = "host.undrained"
	EventKeyCreated  = "enrollment-key.created"
	EventKeyRevoked  = "enrollment-key.revoked"
	EventCodeRefused = "host.admission-refused"
	EventRemoved     = "host.removed"
	EventProfileSet  = "host-profile.set"
	// EventPrefixConflict is a Machine prefix a host reports that another host holds.
	EventPrefixConflict = "host.prefix-conflict"
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
	// Hardware is set by a host that enrolled from the install seed.
	Hardware *orchapi.Hardware `json:"hardware,omitempty"`
}

// Allocation is the Machine prefix given to a host. A non-zero ReservedUntil marks a reservation,
// which an admission holds between asking the signer and writing the host and which another
// admission may take over once it has passed; confirming the allocation clears it.
type Allocation struct {
	Prefix        string    `json:"prefix"`
	HostID        string    `json:"hostId"`
	ReservedUntil time.Time `json:"reservedUntil,omitzero"`
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
	Draining    bool              `json:"draining,omitempty"`
	// MachinePrefix is the prefix orch allocated for a host that joined from the install seed, and
	// empty for a host enrolled with its own configuration. Orch builds the naru.yaml of a joined host
	// on each answer from this record and the current profile, so no credential is stored here.
	MachinePrefix string `json:"machinePrefix,omitempty"`
	// Config is what records written before ADR 0066 carried: the naru.yaml with the profile's
	// credential in it. It is read only so that the migration can drop it, and never written.
	Config []byte `json:"config,omitempty"`
}

func (h Host) api() orchapi.Host {
	labels := h.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return orchapi.Host{ID: h.ID, Region: h.Region, Labels: labels, Address: h.Address, AdmittedAt: h.AdmittedAt, LastSeen: h.LastSeen, Draining: h.Draining, MachinePrefix: h.MachinePrefix}
}
