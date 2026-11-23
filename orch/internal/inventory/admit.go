package inventory

import (
	"context"
	"crypto"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

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
	now := s.clock.Now()
	h := Host{ID: p.HostID, PublicKey: p.PublicKey, Region: req.Region, Labels: req.Labels, Address: p.Address, AdmittedAt: now}
	ev := orchapi.Event{Time: now, Type: EventAdmitted, Subject: p.HostID, Detail: map[string]string{"by": by, "region": req.Region}}
	if p.Hardware == nil {
		if h.Certificate, err = s.issue(ctx, req.Statement, p); err != nil {
			return orchapi.Host{}, err
		}
		if err := s.store.Admit(ctx, p, h, ev, nil); err != nil {
			return orchapi.Host{}, err
		}
		return h.api(), nil
	}
	// The pool hands out only a prefix no admitted host holds, so every admitted host's prefix must be
	// on record before it is asked: a host enrolled with its own configuration chose its prefix itself.
	if err := s.requireRecorded(ctx); err != nil {
		return orchapi.Host{}, err
	}
	// A joined host takes its prefix first, so a full pool is refused before the signer issues
	// anything. The reservation holds the prefix while the signer is asked; one transaction then
	// writes the host and confirms the prefix, and any failure before that drops the reservation.
	res, err := s.store.Reserve(ctx, h.ID, func(claimed []netip.Prefix) (netip.Prefix, error) {
		return nextPrefix(joined.profile.MachinePool, claimed)
	}, now.Add(reservationTTL), now)
	if err != nil {
		return orchapi.Host{}, err
	}
	released := false
	defer func() {
		if !released {
			// The admission may have been cancelled, and the reservation is dropped regardless.
			_ = s.store.Release(context.WithoutCancel(ctx), res)
		}
	}()
	if h.Certificate, err = s.issue(ctx, req.Statement, p); err != nil {
		return orchapi.Host{}, err
	}
	prefix := netip.MustParsePrefix(res.Prefix)
	h.MachinePrefix = res.Prefix
	// The configuration is built here only to refuse an admission it cannot be built for; it is not
	// stored, and is built again from the record on each answer.
	if _, err := joined.buildConfig(h, prefix, s.conf.OrchCA, s.conf.RevocationPublicKey); err != nil {
		return orchapi.Host{}, err
	}
	ev.Detail["machinePrefix"] = h.MachinePrefix
	if err := s.store.Admit(ctx, p, h, ev, &res); err != nil {
		return orchapi.Host{}, err
	}
	released = true
	return h.api(), nil
}

// requireRecorded is ErrPrefixUnrecorded naming each admitted host that holds no confirmed Machine
// prefix in the allocation records, and nil when every one holds one.
func (s *service) requireRecorded(ctx context.Context) error {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return err
	}
	allocations, err := s.store.ListAllocations(ctx)
	if err != nil {
		return err
	}
	recorded := map[string]bool{}
	for _, a := range allocations {
		if a.ReservedUntil.IsZero() {
			recorded[a.HostID] = true
		}
	}
	var missing []string
	for _, h := range hosts {
		if !recorded[h.ID] {
			missing = append(missing, h.ID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("%w: %s; admission waits until each reports its prefix, or the host is removed", ErrPrefixUnrecorded, strings.Join(missing, ", "))
}

// reservationTTL is how long an admission may hold a prefix before it commits. An admission that
// stops between the two steps leaves the reservation, which the next admission takes over once
// this has passed.
const reservationTTL = 2 * time.Minute

// issue asks the signer for the host's certificate and checks it is for the host's key.
func (s *service) issue(ctx context.Context, statement []byte, p Pending) ([]byte, error) {
	cert, err := s.signer.IssueHost(ctx, statement, p.CSR)
	if err != nil {
		if errors.Is(err, ErrSignerRefused) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrSignerFailed, err)
	}
	if err := certificateIsFor(cert, p.PublicKey); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignerFailed, err)
	}
	return cert, nil
}

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
