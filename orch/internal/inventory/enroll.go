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

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func (s *service) Enroll(ctx context.Context, source string, req orchapi.EnrollRequest) (orchapi.EnrollStatus, error) {
	if !s.sources.allow(source) {
		return orchapi.EnrollStatus{}, ErrRateLimited
	}
	key, err := s.usableKey(ctx, req.Key)
	if err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if !s.keys.allow(key.ID) {
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

	if h, err := s.store.GetHost(ctx, req.HostID); err == nil {
		if !bytes.Equal(h.PublicKey, req.PublicKey) {
			return orchapi.EnrollStatus{}, ErrConflict
		}
		return orchapi.EnrollStatus{State: orchapi.StateAdmitted, Certificate: h.Certificate}, nil
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
		p.Address = req.Address
		return orchapi.EnrollStatus{State: orchapi.StatePending, Code: code}, s.store.PutPending(ctx, p, nil)
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
	p = Pending{HostID: req.HostID, PublicKey: req.PublicKey, CSR: req.CSR, Address: req.Address, KeyID: key.ID, FirstSeen: now}
	ev := orchapi.Event{Time: now, Type: EventEnrolling, Subject: req.HostID, Detail: map[string]string{"keyId": key.ID, "code": code, "address": req.Address}}
	return orchapi.EnrollStatus{State: orchapi.StatePending, Code: code}, s.store.PutPending(ctx, p, &ev)
}

// usableKey finds the key a secret names and refuses it when it is unknown, expired or revoked.
func (s *service) usableKey(ctx context.Context, secret string) (Key, error) {
	hash := hashSecret(secret)
	k, err := s.store.GetKey(ctx, keyID(hash))
	if errors.Is(err, ErrNotFound) || (err == nil && subtle.ConstantTimeCompare([]byte(k.Hash), []byte(hash)) != 1) {
		return Key{}, ErrKeyRefused
	}
	if err != nil {
		return Key{}, err
	}
	switch {
	case k.Revoked:
		return Key{}, fmt.Errorf("%w: the key is revoked", ErrKeyRefused)
	case !s.clock.Now().Before(k.ExpiresAt):
		return Key{}, fmt.Errorf("%w: the key is expired", ErrKeyRefused)
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

func (s *service) Poll(ctx context.Context, hostID, timeValue, signature string) (orchapi.EnrollStatus, error) {
	if h, err := s.store.GetHost(ctx, hostID); err == nil {
		if err := orchapi.VerifyProof(h.PublicKey, hostID, timeValue, signature, s.clock.Now()); err != nil {
			return orchapi.EnrollStatus{}, err
		}
		return orchapi.EnrollStatus{State: orchapi.StateAdmitted, Certificate: h.Certificate}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return orchapi.EnrollStatus{}, err
	}
	p, err := s.store.GetPending(ctx, hostID)
	if err != nil {
		return orchapi.EnrollStatus{}, err
	}
	if err := orchapi.VerifyProof(p.PublicKey, hostID, timeValue, signature, s.clock.Now()); err != nil {
		return orchapi.EnrollStatus{}, err
	}
	return orchapi.EnrollStatus{State: orchapi.StatePending, Code: identity.PairingCode(p.PublicKey)}, nil
}
