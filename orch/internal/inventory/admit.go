package inventory

import (
	"context"
	"crypto"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func (s *service) Admit(ctx context.Context, by string, req orchapi.AdmitRequest) (orchapi.Host, error) {
	if err := validHostID(req.HostID); err != nil {
		return orchapi.Host{}, err
	}
	if err := validAttributes(req.Region, req.Labels); err != nil {
		return orchapi.Host{}, err
	}
	if len(req.Statement) == 0 {
		return orchapi.Host{}, invalid("the admission statement is empty")
	}
	p, err := s.store.GetPending(ctx, req.HostID)
	if err != nil {
		return orchapi.Host{}, err
	}
	var joined joinedPlan
	if p.Hardware != nil {
		// A host that enrolled from the install seed has no configuration of its own: orch builds
		// it, so what it is built from is checked before the signer is asked for a certificate.
		if joined, err = s.planJoined(ctx, p); err != nil {
			return orchapi.Host{}, err
		}
	}
	// The code is orch's own computation from the stored key, never a value the host sent.
	want := normalizeCode(identity.PairingCode(p.PublicKey))
	if subtle.ConstantTimeCompare([]byte(want), []byte(normalizeCode(req.Code))) != 1 {
		ev := orchapi.Event{Time: s.clock.Now(), Type: EventCodeRefused, Subject: req.HostID, Detail: map[string]string{"by": by}}
		if err := s.store.AppendEvent(ctx, ev); err != nil {
			return orchapi.Host{}, err
		}
		return orchapi.Host{}, ErrCodeMismatch
	}
	cert, err := s.signer.IssueHost(ctx, req.Statement, p.CSR)
	if err != nil {
		if errors.Is(err, ErrSignerRefused) {
			return orchapi.Host{}, err
		}
		return orchapi.Host{}, fmt.Errorf("%w: %v", ErrSignerFailed, err)
	}
	if err := certificateIsFor(cert, p.PublicKey); err != nil {
		return orchapi.Host{}, fmt.Errorf("%w: %v", ErrSignerFailed, err)
	}
	now := s.clock.Now()
	h := Host{ID: p.HostID, PublicKey: p.PublicKey, Certificate: cert, Region: req.Region, Labels: req.Labels, Address: p.Address, AdmittedAt: now}
	ev := orchapi.Event{Time: now, Type: EventAdmitted, Subject: p.HostID, Detail: map[string]string{"by": by, "region": req.Region}}
	if p.Hardware == nil {
		if err := s.store.Admit(ctx, p, h, ev, nil); err != nil {
			return orchapi.Host{}, err
		}
		return h.api(), nil
	}
	// Another admission may take the prefix between the read of the allocations and the write, which
	// the store refuses; the next prefix is then tried with the certificate already issued.
	for range admitRetries {
		prefix, err := s.nextPrefix(ctx, joined.profile.MachinePool)
		if err != nil {
			return orchapi.Host{}, err
		}
		if h.Config, err = joined.buildConfig(h, prefix, s.conf.OrchCA); err != nil {
			return orchapi.Host{}, err
		}
		h.MachinePrefix = prefix.String()
		ev.Detail["machinePrefix"] = h.MachinePrefix
		err = s.store.Admit(ctx, p, h, ev, &Allocation{Prefix: h.MachinePrefix, HostID: h.ID})
		if errors.Is(err, ErrAllocationTaken) {
			continue
		}
		if err != nil {
			return orchapi.Host{}, err
		}
		return h.api(), nil
	}
	return orchapi.Host{}, ErrConflict
}

// admitRetries bounds the admissions that lost the race for a prefix.
const admitRetries = 8

// certificateIsFor checks that certPEM holds a certificate for the key a host enrolled with.
func certificateIsFor(certPEM, publicKeyDER []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("the signer returned no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("the signer returned a certificate that does not parse")
	}
	pub, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return err
	}
	if k, ok := pub.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(cert.PublicKey) {
		return errors.New("the signer returned a certificate for another key than the host's")
	}
	return nil
}
