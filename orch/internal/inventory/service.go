package inventory

import (
	"context"
	"net/netip"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// Service is what the API offers on the inventory. The roles that may call each method are the
// API's to check; the service takes the caller's subject only to record it.
type Service interface {
	// Enroll takes a host's request, which carries an enrollment key and nothing else that
	// authenticates it, and leaves the host pending. source names where it came from.
	Enroll(ctx context.Context, source string, req orchapi.EnrollRequest) (orchapi.EnrollStatus, error)
	// Poll answers an enrolling host that proves it holds the key it enrolled with. source names
	// where it came from. An unknown host and a bad proof both answer ErrNotFound, and a source over
	// its limit ErrRateLimited.
	Poll(ctx context.Context, source, hostID, timeValue, signature string) (orchapi.EnrollStatus, error)
	Admit(ctx context.Context, by string, req orchapi.AdmitRequest) (orchapi.Host, error)
	CreateKey(ctx context.Context, by string, expiresAt time.Time) (orchapi.EnrollmentKey, error)
	RevokeKey(ctx context.Context, by, id string) error
	SetLabels(ctx context.Context, by, hostID string, req orchapi.SetLabelsRequest) (orchapi.Host, error)
	// Drain and Undrain set whether the groups engine may place on the host and keep members on it.
	Drain(ctx context.Context, by, hostID string) (orchapi.Host, error)
	Undrain(ctx context.Context, by, hostID string) (orchapi.Host, error)
	// Heartbeat records that the admitted host hostID is alive.
	Heartbeat(ctx context.Context, hostID string) error
	Hosts(ctx context.Context) ([]orchapi.Host, error)
	Pending(ctx context.Context) ([]orchapi.Pending, error)
	// Events lists the events after the cursor after, oldest first; an empty after lists them all.
	Events(ctx context.Context, after string) ([]orchapi.Event, error)
	// Remove deletes the host's record and frees its Machine prefix. The host's certificate stays
	// valid: only a person's signature revokes it.
	Remove(ctx context.Context, by, hostID string) error
	// Profile returns the host profile's YAML with every secret redacted, ErrNotFound when none is
	// set. SetProfile checks the profile and replaces it; a profile that carries the redaction is
	// ErrRedacted.
	Profile(ctx context.Context) ([]byte, error)
	SetProfile(ctx context.Context, by string, yaml []byte) error
}

// Config sets the service's limits. A zero value is replaced by its default.
type Config struct {
	// SourceRate and SourceBurst bound enrollment requests per second per source address, KeyRate
	// and KeyBurst per enrollment key.
	SourceRate  float64
	SourceBurst int
	KeyRate     float64
	KeyBurst    int
	// PollRate and PollBurst bound polls per second per source address; a fleet behind one address
	// polls together, every PollEvery of hostd.
	PollRate  float64
	PollBurst int
	// MaxPending bounds the pending list.
	MaxPending int
	// OrchCA is the PEM of the authority orch's certificate chains to, which a joined host's naru-hostd
	// trusts for orch's calls. It is the authority of the callers' certificates, which orch holds.
	OrchCA string
	// RevocationPublicKey is the PKIX PEM of the key that signs the revocation list. With it a
	// joined host's configuration carries the key and the list orch's signer holds at admission
	// (ADR 0062); empty leaves a joined host with no revocation check.
	RevocationPublicKey string
}

// Claim is a prefix another part of orch holds, and what holds it, such as "network a".
type Claim struct {
	Prefix netip.Prefix
	By     string
}

// AddressSpace lists the prefixes the networks and the address pools hold, which the profile's mesh
// pool is kept apart from.
type AddressSpace interface {
	Claimed(ctx context.Context) ([]Claim, error)
}

type service struct {
	store   Store
	space   AddressSpace
	signer  Signer
	clock   Clock
	conf    Config
	sources *limiter
	keys    *limiter
	polls   *limiter
}

// NewService returns the service over store and signer. space is what a profile's mesh pool is
// checked against.
func NewService(store Store, space AddressSpace, signer Signer, clock Clock, conf Config) Service {
	if conf.SourceRate == 0 {
		conf.SourceRate = 1
	}
	if conf.SourceBurst == 0 {
		conf.SourceBurst = 10
	}
	if conf.KeyRate == 0 {
		conf.KeyRate = 5
	}
	if conf.KeyBurst == 0 {
		conf.KeyBurst = 50
	}
	if conf.PollRate == 0 {
		conf.PollRate = 10
	}
	if conf.PollBurst == 0 {
		conf.PollBurst = 100
	}
	if conf.MaxPending == 0 {
		conf.MaxPending = 1000
	}
	return &service{
		store: store, space: space, signer: signer, clock: clock, conf: conf,
		sources: newLimiter(clock, conf.SourceRate, conf.SourceBurst),
		keys:    newLimiter(clock, conf.KeyRate, conf.KeyBurst),
		polls:   newLimiter(clock, conf.PollRate, conf.PollBurst),
	}
}
