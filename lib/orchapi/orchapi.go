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
	// MachinePrefix is the prefix orch allocated for the Machines of a host that joined from the
	// install seed. It is empty for a host enrolled with its own configuration.
	MachinePrefix string `json:"machinePrefix,omitempty"`
	// MeshAddress is the host's own address on its tunnel, from the profile's mesh pool (ADR 0113
	// item 1). It is empty until orch gives the host one.
	MeshAddress string `json:"meshAddress,omitempty"`
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
	// Hardware is what a host that enrolled from the install seed reported, which an operator reads
	// beside the pairing code to know which machine it is. It is nil for a host enrolled with its own
	// configuration.
	Hardware *Hardware `json:"hardware,omitempty"`
}

// Hardware is what a host that joined reports of itself: its processors, memory, disks and network
// links. Orch keeps it as reported; it is not a claim an admission rests on.
type Hardware struct {
	CPUs        int    `json:"cpus"`
	MemoryBytes uint64 `json:"memoryBytes"`
	Disks       []Disk `json:"disks"`
	Links       []Link `json:"links"`
}

// Disk is a block device of a joining host. Partitions is how many partitions the kernel lists on
// it: a fact the host read, not a judgement of whether the disk is free, which only init's check of
// the join form makes.
type Disk struct {
	Name       string `json:"name"`
	Size       uint64 `json:"size"`
	Model      string `json:"model"`
	Partitions int    `json:"partitions"`
}

// Link is a network interface of a joining host.
type Link struct {
	Name    string `json:"name"`
	MAC     string `json:"mac"`
	Carrier bool   `json:"carrier"`
}

// EnrollRequest is what a host sends with the enrollment key. PublicKey is the PKIX DER of the
// host's own key and CSR the DER certificate request for the same key.
type EnrollRequest struct {
	Key       string `json:"key"`
	HostID    string `json:"hostId"`
	PublicKey []byte `json:"publicKey"`
	CSR       []byte `json:"csr"`
	Address   string `json:"address"`
	// Hardware is set by a host that enrolled from the install seed: it is what tells orch to build
	// the host's configuration at admission instead of expecting the host to have one.
	Hardware *Hardware `json:"hardware,omitempty"`
}

// EnrollStatus answers an enrollment and each poll of it. Certificate, the PEM of the host's
// certificate, is set once State is admitted.
type EnrollStatus struct {
	State       string `json:"state"`
	Code        string `json:"code,omitempty"`
	Certificate []byte `json:"certificate,omitempty"`
	// Config is the host's naru.yaml, which orch built from the host profile at admission. It is set
	// with Certificate for a host that enrolled from the install seed and carries no cluster sections.
	Config []byte `json:"config,omitempty"`
}

// HostProfile is the YAML of the host profile orch keeps: what every joined host shares.
type HostProfile struct {
	Profile []byte `json:"profile"`
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
	// Cursor places the event in the log: orch sets it when it lists events, and a reader that
	// passes the last Cursor it saw to EventsSince is given only what came after.
	Cursor string `json:"cursor,omitempty"`
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
