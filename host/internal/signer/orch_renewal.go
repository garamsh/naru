package signer

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// authoriseOrch verifies a request an orch member's certificate signed, against the orch
// authority and the revocation list (ADR 0123).
func (s *Signer) authoriseOrch(signed []byte, action string, body any) (identity.Identity, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Identity{}, err
	}
	who, raw, err := identity.VerifyRequest(signed, action, pool(a.orch.cert), &a.crl, s.now())
	if err != nil {
		return identity.Identity{}, err
	}
	if who.Role != identity.RoleOrch || !namePattern.MatchString(who.Subject) {
		return identity.Identity{}, fmt.Errorf("%w: %s may not %s", ErrForbidden, who.Role, action)
	}
	return who, decode(raw, body)
}

// RenewOrch issues an orch member a further certificate for the key of the current one that signed
// the request, in the last identity.OrchRenewWindow of that certificate's life (ADR 0123). The new
// certificate has the current one's name, DNS names and IP addresses, whatever the request asks,
// is valid a year, and is recorded among the member's serials beside the one that signed it, so
// the retire that follows its use revokes the others. A member whose answer was lost renews again:
// the renewal it lost is revoked, so a certificate has at most one renewal not retired.
func (s *Signer) RenewOrch(signed []byte) (identity.Issued, error) {
	var req identity.OrchRenewal
	who, err := s.authoriseOrch(signed, identity.ActionRenewOrch, &req)
	if err != nil {
		return identity.Issued{}, err
	}
	now := s.now()
	if due := who.NotAfter.Add(-identity.OrchRenewWindow); now.Before(due) {
		return identity.Issued{}, fmt.Errorf("%w: renew from %s", identity.ErrOrchNotDue, due.Format(time.RFC3339))
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	if k, ok := csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(who.PublicKey) {
		return identity.Issued{}, fmt.Errorf("%w: an orch member renews under the key of its current certificate", ErrBadRequest)
	}

	if renewHook != nil {
		renewHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	// The request was verified against the list as it stood before the lock: a revocation
	// committed since then ends the renewal here, under the lock every revocation takes.
	if cur.crl.Revokes(who.Serial) {
		return identity.Issued{}, fmt.Errorf("%w: the certificate %s was revoked while it renewed", identity.ErrRevoked, who.Serial)
	}
	if role := cur.issued[who.Serial]; role != "" && role != string(identity.RoleOrch) {
		return identity.Issued{}, fmt.Errorf("%w: serial %s is a %s certificate", ErrForbidden, who.Serial, role)
	}
	// A certificate issued before the registry is recorded at its first renewal.
	if !slices.Contains(cur.orchs[who.Subject], who.Serial) {
		cur = cur.withOrch(who.Subject, who.Serial)
	}
	held := cur.orchs[who.Subject]
	var earlier []string
	for _, serial := range held[slices.Index(held, who.Serial)+1:] {
		if !cur.crl.Revokes(serial) {
			earlier = append(earlier, serial)
		}
	}
	issued, err := issueLeaf(cur.orch, now, who.Subject, identity.RoleOrch, csr.PublicKey, serverAndClient,
		&x509.CertificateRequest{DNSNames: who.DNSNames, IPAddresses: who.IPAddresses})
	if err != nil {
		return identity.Issued{}, err
	}
	next := cur.withOrch(who.Subject, issued.Serial)
	files := []stagedFile{{issuedFile, next.marshalIssued()}, {orchsFile, next.marshalOrchs()}}
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
	s.log.Info("renewed an orch member's certificate", "name", who.Subject, "serial", issued.Serial, "previous_serial", who.Serial, "revoked_earlier_renewals", earlier)
	return issued, nil
}

// RetireOwnOrch revokes every serial the signer holds for an orch member but the one of the
// certificate that signed the request: the member's renewed certificate, which it signs with once
// it serves with it (ADR 0123). A repeat changes nothing.
func (s *Signer) RetireOwnOrch(signed []byte) error {
	var body struct{}
	who, err := s.authoriseOrch(signed, identity.ActionRetireOwnOrch, &body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	held := cur.orchs[who.Subject]
	if !slices.Contains(held, who.Serial) {
		return fmt.Errorf("%w: serial %s is not one of %s's", ErrUnknownHost, who.Serial, who.Subject)
	}
	if len(held) == 1 {
		return nil
	}
	revoke := slices.DeleteFunc(slices.Clone(held), func(serial string) bool { return serial == who.Serial || cur.crl.Revokes(serial) })
	now := s.now()
	next := cur.withOrchs(who.Subject, []string{who.Serial})
	files := []stagedFile{{orchsFile, next.marshalOrchs()}}
	crl := cur.successor(now)
	if len(revoke) > 0 {
		crl.Serials = slices.Sorted(slices.Values(slices.Concat(crl.Serials, revoke)))
		if err := next.signCRL(crl); err != nil {
			return err
		}
		files = append(files, stagedFile{crlFile, next.crlSigned})
	}
	if err := commitFiles(s.dir, files); err != nil {
		return err
	}
	s.auth = next
	s.log.Info("retired an orch member's earlier certificates", "name", who.Subject, "kept", who.Serial, "revoked", revoke, "seq", crl.Seq)
	return nil
}
