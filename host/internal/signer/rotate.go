package signer

import (
	"crypto/x509"
	"fmt"
	"math/big"
	"slices"

	"github.com/garamsh/naru/lib/identity"
)

// RotateHost issues a new host certificate, for the key in the certificate request of a request
// root or an operator signed, to the host it names, and puts every serial the signer holds for that
// host on the revocation list. The new certificate, the revocation list and the record of the host
// reach the directory as one commit, so a crash leaves the old certificate valid or the new one
// issued with the old one revoked, never the old one revoked with no new one.
func (s *Signer) RotateHost(signed []byte) (identity.Issued, error) {
	var req identity.RotateHostRequest
	who, _, err := s.authorise(signed, identity.ActionRotateHost, &req, identity.RoleRoot, identity.RoleOperator)
	if err != nil {
		return identity.Issued{}, err
	}
	if !namePattern.MatchString(req.HostID) {
		return identity.Issued{}, fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	old := cur.hosts[req.HostID]
	if len(old) == 0 {
		return identity.Issued{}, fmt.Errorf("%w: %s; register the serial of an offline pair with register-host", ErrUnknownHost, req.HostID)
	}
	now := s.now()
	issued, err := issueLeaf(cur.host, now, req.HostID, identity.RoleHost, csr.PublicKey, serverAndClient, &x509.CertificateRequest{DNSNames: []string{req.HostID}})
	if err != nil {
		return identity.Issued{}, err
	}
	next := cur.withHost(req.HostID, issued.Serial)
	next.hosts[req.HostID] = []string{issued.Serial}
	crl := cur.successor(now)
	crl.Serials = slices.Sorted(slices.Values(slices.Concat(crl.Serials, slices.DeleteFunc(slices.Clone(old), cur.crl.Revokes))))
	if err := next.signCRL(crl); err != nil {
		return identity.Issued{}, err
	}
	if err := commitFiles(s.dir, []stagedFile{{issuedFile, next.marshalIssued()}, {hostsFile, next.marshalHosts()}, {crlFile, next.crlSigned}}); err != nil {
		return identity.Issued{}, err
	}
	s.auth = next
	s.log.Info("rotated a host's certificate", "host_id", req.HostID, "serial", issued.Serial, "revoked", old, "seq", crl.Seq,
		"rotated_by", who.Subject, "rotated_by_serial", who.Serial)
	return issued, nil
}

// RegisterHost records the serial of a certificate another authority issued to a host, such as the
// offline pair gen node writes, so that a rotation of the host revokes it. The signer cannot check
// the certificate: it takes the serial from the operator who signed the request.
func (s *Signer) RegisterHost(signed []byte) error {
	var req identity.RegisterHostRequest
	who, _, err := s.authorise(signed, identity.ActionRegisterHost, &req, identity.RoleRoot, identity.RoleOperator)
	if err != nil {
		return err
	}
	if !namePattern.MatchString(req.HostID) {
		return fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}
	n, ok := new(big.Int).SetString(req.Serial, 16)
	if !ok || n.Sign() <= 0 {
		return fmt.Errorf("%w: serial %q is not hexadecimal", ErrBadRequest, req.Serial)
	}
	serial := n.Text(16)

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	if role := cur.issued[serial]; role != "" && role != string(identity.RoleHost) {
		return fmt.Errorf("%w: serial %s is a %s certificate, not a host's", ErrBadRequest, serial, role)
	}
	if slices.Contains(cur.hosts[req.HostID], serial) {
		return nil
	}
	next := cur.withHost(req.HostID, serial)
	if err := commitFiles(s.dir, []stagedFile{{issuedFile, next.marshalIssued()}, {hostsFile, next.marshalHosts()}}); err != nil {
		return err
	}
	s.auth = next
	s.log.Info("registered a host's certificate", "host_id", req.HostID, "serial", serial, "registered_by", who.Subject, "registered_by_serial", who.Serial)
	return nil
}
