package inventory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
)

// MaxKeyLife is the longest an enrollment key may live, as a caller's registration token does
// (ADR 0121).
const MaxKeyLife = 24 * time.Hour

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// keyID is the record's name: the first 16 hex digits of the secret's hash, which reveal nothing
// of a 256-bit secret.
func keyID(hash string) string { return hash[:16] }

func (s *service) CreateKey(ctx context.Context, by string, expiresAt time.Time) (orchapi.EnrollmentKey, error) {
	now := s.clock.Now()
	if !expiresAt.After(now) || expiresAt.After(now.Add(MaxKeyLife)) {
		return orchapi.EnrollmentKey{}, invalid("an enrollment key expires within the next 24 hours")
	}
	var raw [wire.EnrollKeyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return orchapi.EnrollmentKey{}, err
	}
	secret := wire.EnrollKey(raw)
	hash := hashSecret(secret)
	k := Key{ID: keyID(hash), Hash: hash, CreatedBy: by, CreatedAt: now, ExpiresAt: expiresAt}
	if err := s.store.PutKey(ctx, k); err != nil {
		return orchapi.EnrollmentKey{}, err
	}
	ev := orchapi.Event{Time: now, Type: EventKeyCreated, Subject: k.ID, Detail: map[string]string{"by": by, "expiresAt": expiresAt.UTC().Format(time.RFC3339)}}
	if err := s.store.AppendEvent(ctx, ev); err != nil {
		return orchapi.EnrollmentKey{}, err
	}
	return orchapi.EnrollmentKey{ID: k.ID, Secret: secret, ExpiresAt: expiresAt}, nil
}

func (s *service) RevokeKey(ctx context.Context, by, id string) error {
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventKeyRevoked, Subject: id, Detail: map[string]string{"by": by}}
	return s.store.RevokeKey(ctx, id, ev)
}
