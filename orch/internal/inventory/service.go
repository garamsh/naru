package inventory

import (
	"context"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// Service is what the API offers on the inventory. The roles that may call each method are the
// API's to check; the service takes the caller's subject only to record it.
type Service interface {
	// Enroll takes a host's request, which carries an enrollment key and nothing else that
	// authenticates it, and leaves the host pending. source names where it came from.
	Enroll(ctx context.Context, source string, req orchapi.EnrollRequest) (orchapi.EnrollStatus, error)
	// Poll answers an enrolling host that proves it holds the key it enrolled with.
	Poll(ctx context.Context, hostID, timeValue, signature string) (orchapi.EnrollStatus, error)
	Admit(ctx context.Context, by string, req orchapi.AdmitRequest) (orchapi.Host, error)
	CreateKey(ctx context.Context, by string, expiresAt time.Time) (orchapi.EnrollmentKey, error)
	RevokeKey(ctx context.Context, by, id string) error
	SetLabels(ctx context.Context, by, hostID string, req orchapi.SetLabelsRequest) (orchapi.Host, error)
	// Heartbeat records that the admitted host hostID is alive.
	Heartbeat(ctx context.Context, hostID string) error
	Hosts(ctx context.Context) ([]orchapi.Host, error)
	Pending(ctx context.Context) ([]orchapi.Pending, error)
	Events(ctx context.Context) ([]orchapi.Event, error)
}

// Config sets the service's limits. A zero value is replaced by its default.
type Config struct {
	// SourceRate and SourceBurst bound enrollment requests per second per source address, KeyRate
	// and KeyBurst per enrollment key.
	SourceRate  float64
	SourceBurst int
	KeyRate     float64
	KeyBurst    int
	// MaxPending bounds the pending list.
	MaxPending int
}

type service struct {
	store   Store
	signer  Signer
	clock   Clock
	conf    Config
	sources *limiter
	keys    *limiter
}

// NewService returns the service over store and signer.
func NewService(store Store, signer Signer, clock Clock, conf Config) Service {
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
	if conf.MaxPending == 0 {
		conf.MaxPending = 1000
	}
	return &service{
		store: store, signer: signer, clock: clock, conf: conf,
		sources: newLimiter(clock, conf.SourceRate, conf.SourceBurst),
		keys:    newLimiter(clock, conf.KeyRate, conf.KeyBurst),
	}
}
