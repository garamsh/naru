package signer

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// authoriseHost verifies a request a host's certificate signed, against the host authority and the
// revocation list (ADR 0122): the host authority, not the operator's, issues a host's certificate.
func (s *Signer) authoriseHost(signed []byte, action string, body any) (identity.Identity, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Identity{}, err
	}
	who, raw, err := identity.VerifyRequest(signed, action, pool(a.host.cert), &a.crl, s.now())
	if err != nil {
		return identity.Identity{}, err
	}
	if who.Role != identity.RoleHost || !namePattern.MatchString(who.Subject) {
		return identity.Identity{}, fmt.Errorf("%w: %s may not %s", ErrForbidden, who.Role, action)
	}
	return who, decode(raw, body)
}

// RenewHost issues a host a further certificate for the key of the current one that signed the
// request, in the last identity.HostRenewWindow of that certificate's life (ADR 0122). The new
// certificate has the current one's host ID and IP addresses, whatever the request asks, is valid a
// year, and is recorded among the host's serials, so the retire that follows its use revokes the
// others. A host whose answer was lost renews again: the renewal it lost is revoked, so a
// certificate has at most one renewal the host has not retired.
func (s *Signer) RenewHost(signed []byte) (identity.Issued, error) {
	var req identity.HostRenewal
	who, err := s.authoriseHost(signed, identity.ActionRenewHost, &req)
	if err != nil {
		return identity.Issued{}, err
	}
	now := s.now()
	if due := who.NotAfter.Add(-identity.HostRenewWindow); now.Before(due) {
		return identity.Issued{}, fmt.Errorf("%w: renew from %s", identity.ErrHostNotDue, due.Format(time.RFC3339))
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	if k, ok := csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(who.PublicKey) {
		return identity.Issued{}, fmt.Errorf("%w: a host renews under the key of its current certificate", ErrBadRequest)
	}

	if renewHook != nil {
		renewHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	// The request was verified against the list as it stood before the lock: a revocation of the
	// host committed since then ends the renewal here, under the lock every revocation takes.
	held := cur.hosts[who.Subject]
	if cur.crl.Revokes(who.Serial) {
		return identity.Issued{}, fmt.Errorf("%w: the certificate %s was revoked while it renewed", identity.ErrRevoked, who.Serial)
	}
	if !slices.Contains(held, who.Serial) {
		return identity.Issued{}, fmt.Errorf("%w: serial %s is not one of %s's", ErrUnknownHost, who.Serial, who.Subject)
	}
	// One unretired renewal per certificate: a renewal the certificate signed before, whose answer
	// was lost or which the host has not retired yet, is revoked with this one's issue.
	var earlier []string
	for _, serial := range held[slices.Index(held, who.Serial)+1:] {
		if !cur.crl.Revokes(serial) {
			earlier = append(earlier, serial)
		}
	}
	issued, err := issueLeaf(cur.host, now, who.Subject, identity.RoleHost, csr.PublicKey, serverAndClient,
		&x509.CertificateRequest{DNSNames: []string{who.Subject}, IPAddresses: who.IPAddresses})
	if err != nil {
		return identity.Issued{}, err
	}
	next := cur.withHost(who.Subject, issued.Serial)
	files := []stagedFile{{issuedFile, next.marshalIssued()}, {hostsFile, next.marshalHosts()}}
	if len(earlier) > 0 {
		crl := cur.successor(now)
		crl.Serials = slices.Sorted(slices.Values(slices.Concat(crl.Serials, earlier)))
		if err := next.signCRL(crl); err != nil {
			return identity.Issued{}, err
		}
		files = append(files, stagedFile{crlFile, next.crlSigned})
	}
	if err := commitFiles(s.dir, files); err != nil {
		return identity.Issued{}, err
	}
	s.auth = next
	s.log.Info("renewed a host's certificate", "host_id", who.Subject, "serial", issued.Serial, "previous_serial", who.Serial, "revoked_earlier_renewals", earlier)
	return issued, nil
}

// renewHook, when set by a test, runs in RenewHost between the request's verification and the lock.
var renewHook func()

// RetireOwnHost revokes every serial the signer holds for a host but the one of the certificate
// that signed the request: the host's renewed certificate, which it signs with once it serves and
// reports with it (ADR 0122). It is RetireHost with the host's word for the serial it keeps.
func (s *Signer) RetireOwnHost(signed []byte) error {
	var body struct{}
	who, err := s.authoriseHost(signed, identity.ActionRetireOwnHost, &body)
	if err != nil {
		return err
	}
	return s.retire(who.Subject, who.Serial, who)
}
