package inventory

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func (s *service) Enroll(ctx context.Context, source string, req orchapi.EnrollRequest) (orchapi.EnrollStatus, error) {
	if !s.sources.Allow(source) {
		return orchapi.EnrollStatus{}, ErrRateLimited
	}
	key, err := s.usableKey(ctx, req)
	if err != nil {
		return orchapi.EnrollStatus{}, s.refused(ctx, key, req, err)
	}
	if !s.keys.Allow(key.ID) {
		return orchapi.EnrollStatus{}, ErrRateLimited
	}
	if err := validHostID(req.HostID); err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if err := checkHostKey(req.PublicKey, req.CSR); err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if len(req.Address) > 255 {
		return orchapi.EnrollStatus{}, invalid("the address is longer than 255 bytes")
	}
	if req.Hardware != nil {
		if err := checkHardware(*req.Hardware); err != nil {
			return orchapi.EnrollStatus{}, err
		}
		if a, err := hostAddress(req.Address); err != nil || !a.Is4() {
			return orchapi.EnrollStatus{}, invalid("the address %q is not an IPv4 address", req.Address)
		}
	}

	// The writes below hold only while the host is not admitted and the key is as it was read, so
	// a request that meets an admission or another enrollment in between starts over: it reads the
	// key again, which the admission spent or the other enrollment bound, and is refused. A host
	// whose admission it missed polls for its certificate instead (ADR 0121 item 8).
	for i := range enrollRetries {
		if i > 0 {
			if key, err = s.usableKey(ctx, req); err != nil {
				return orchapi.EnrollStatus{}, s.refused(ctx, key, req, err)
			}
		}
		st, err := s.enrollOnce(ctx, key, req)
		if errors.Is(err, ErrAlreadyAdmitted) || (errors.Is(err, ErrConflict) && errors.Is(err, errRaced)) {
			continue
		}
		return st, s.refused(ctx, key, req, err)
	}
	return orchapi.EnrollStatus{}, ErrConflict
}

// enrollRetries bounds the starts over of an enrollment that lost a race with another write.
const enrollRetries = 4

// errRaced marks a conflict that is a lost race, not a key mismatch.
var errRaced = errors.New("the enrollment changed meanwhile")

func (s *service) enrollOnce(ctx context.Context, key Key, req orchapi.EnrollRequest) (orchapi.EnrollStatus, error) {
	if h, err := s.store.GetHost(ctx, req.HostID); err == nil {
		if !bytes.Equal(h.PublicKey, req.PublicKey) {
			return orchapi.EnrollStatus{}, ErrConflict
		}
		// A key the admitted host presents is spent with it, so it enrolls no other host.
		if err := s.store.SpendKey(ctx, key.ID, req.HostID, req.PublicKey); err != nil {
			return orchapi.EnrollStatus{}, raced(err)
		}
		cfg, err := s.configFor(ctx, h)
		if err != nil {
			return orchapi.EnrollStatus{}, err
		}
		return orchapi.EnrollStatus{State: orchapi.StateAdmitted, Certificate: h.Certificate, Config: cfg}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return orchapi.EnrollStatus{}, err
	}

	code := identity.PairingCode(req.PublicKey)
	p, err := s.store.GetPending(ctx, req.HostID)
	switch {
	case err == nil:
		if !bytes.Equal(p.PublicKey, req.PublicKey) {
			return orchapi.EnrollStatus{}, ErrConflict
		}
		p.Address, p.Hardware = req.Address, req.Hardware
		return orchapi.EnrollStatus{State: orchapi.StatePending, Code: code}, raced(s.store.PutPending(ctx, p, nil, false))
	case !errors.Is(err, ErrNotFound):
		return orchapi.EnrollStatus{}, err
	}
	all, err := s.store.ListPending(ctx)
	if err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if len(all) >= s.conf.MaxPending {
		return orchapi.EnrollStatus{}, ErrTooManyPending
	}
	now := s.clock.Now()
	p = Pending{HostID: req.HostID, PublicKey: req.PublicKey, CSR: req.CSR, Address: req.Address, KeyID: key.ID, FirstSeen: now, Hardware: req.Hardware}
	ev := orchapi.Event{Time: now, Type: EventEnrolling, Subject: req.HostID, Detail: map[string]string{"keyId": key.ID, "code": code, "address": req.Address}}
	return orchapi.EnrollStatus{State: orchapi.StatePending, Code: code}, raced(s.store.PutPending(ctx, p, &ev, true))
}

// raced marks the store's conflict as a lost race.
func raced(err error) error {
	if errors.Is(err, ErrConflict) {
		return fmt.Errorf("%w: %w", ErrConflict, errRaced)
	}
	return err
}

// refused records the refusal of a stored key as an EventKeyRefused, within the key's rate, and
// returns err; any other error is returned as it is. The host is answered ErrKeyRefused alone
// (ADR 0121 item 8), and an unknown key, which names no stored key, is not an event.
func (s *service) refused(ctx context.Context, key Key, req orchapi.EnrollRequest, err error) error {
	if !errors.Is(err, ErrKeyRefused) || key.ID == "" {
		return err
	}
	if !s.keys.Allow(key.ID) {
		return ErrRateLimited
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventKeyRefused, Subject: key.ID, Detail: map[string]string{"hostId": req.HostID, "reason": err.Error()}}
	if aerr := s.store.AppendEvent(ctx, ev); aerr != nil {
		return fmt.Errorf("%w (and its event was not appended: %w)", err, aerr)
	}
	return err
}

// usableKey finds the key req's secret names and refuses it when it is unknown, expired, revoked,
// spent, or bound to another host or public key than req's. Every refusal is ErrKeyRefused, which
// the API answers with one body; a refusal of a stored key returns the key too, for its event.
func (s *service) usableKey(ctx context.Context, req orchapi.EnrollRequest) (Key, error) {
	hash := hashSecret(req.Key)
	k, err := s.store.GetKey(ctx, keyID(hash))
	if errors.Is(err, ErrNotFound) || (err == nil && subtle.ConstantTimeCompare([]byte(k.Hash), []byte(hash)) != 1) {
		return Key{}, ErrKeyRefused
	}
	if err != nil {
		return Key{}, err
	}
	switch {
	case k.Revoked:
		return k, fmt.Errorf("%w: the key is revoked", ErrKeyRefused)
	case !s.clock.Now().Before(k.lapses()):
		return k, fmt.Errorf("%w: the key is expired", ErrKeyRefused)
	case k.Spent:
		return k, fmt.Errorf("%w: the key is spent: its host is admitted", ErrKeyRefused)
	case !k.enrolls(req.HostID, req.PublicKey):
		return k, fmt.Errorf("%w: the key enrolled another host or key", ErrKeyRefused)
	}
	return k, nil
}

// checkHostKey requires an ECDSA or Ed25519 PKIX key and a certificate request for that same key.
func checkHostKey(publicKeyDER, csrDER []byte) error {
	pub, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return invalid("the public key is not PKIX DER")
	}
	switch pub.(type) {
	case *ecdsa.PublicKey, ed25519.PublicKey:
	default:
		return invalid("the host key is a %T; ECDSA and Ed25519 are accepted", pub)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return invalid("the certificate request is not DER")
	}
	if csr.CheckSignature() != nil {
		return invalid("the certificate request's signature does not hold")
	}
	if k, ok := csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(pub) {
		return invalid("the certificate request is for another key than the public key")
	}
	return nil
}

// Poll answers an enrolling host. An unknown host and a known one with a proof that does not hold
// get the same ErrNotFound, so the answer does not tell which host IDs exist.
func (s *service) Poll(ctx context.Context, source, hostID, timeValue, signature string) (orchapi.EnrollStatus, error) {
	if !s.polls.Allow(source) {
		return orchapi.EnrollStatus{}, ErrRateLimited
	}
	if h, err := s.store.GetHost(ctx, hostID); err == nil {
		if err := orchapi.VerifyProof(h.PublicKey, hostID, timeValue, signature, s.clock.Now()); err != nil {
			return orchapi.EnrollStatus{}, ErrNotFound
		}
		cfg, err := s.configFor(ctx, h)
		if err != nil {
			return orchapi.EnrollStatus{}, err
		}
		return orchapi.EnrollStatus{State: orchapi.StateAdmitted, Certificate: h.Certificate, Config: cfg}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return orchapi.EnrollStatus{}, err
	}
	p, err := s.store.GetPending(ctx, hostID)
	if err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if err := orchapi.VerifyProof(p.PublicKey, hostID, timeValue, signature, s.clock.Now()); err != nil {
		return orchapi.EnrollStatus{}, ErrNotFound
	}
	return orchapi.EnrollStatus{State: orchapi.StatePending, Code: identity.PairingCode(p.PublicKey)}, nil
}

// configFor builds the naru.yaml of an admitted host that joined from the install seed from its
// record and the current profile, and returns nil for a host enrolled with its own configuration.
// A profile change shows in the next answer, and the profile's credential is never copied.
func (s *service) configFor(ctx context.Context, h Host) ([]byte, error) {
	if h.MachinePrefix == "" {
		return nil, nil
	}
	prefix, err := netip.ParsePrefix(h.MachinePrefix)
	if err != nil {
		return nil, fmt.Errorf("%w: the host's machine prefix %q: %v", ErrConflict, h.MachinePrefix, err)
	}
	j, err := s.planJoined(ctx, Pending{Address: h.Address})
	if err != nil {
		return nil, err
	}
	return j.buildConfig(h, prefix, s.conf.OrchCA, s.conf.RevocationPublicKey)
}
