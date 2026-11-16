package signer

import (
	"crypto/x509"
	"fmt"
	"math/big"
	"slices"

	"github.com/garamsh/naru/lib/identity"
)

// ReissueHost issues a further host certificate, for the key in the certificate request of a request
// root or an operator signed, to the host it names, and records its serial beside the ones the host
// holds. It revokes nothing: the certificates the host holds stay valid until RetireHost, so a
// rotation can deliver the new pair before it ends the old ones. A new certificate the host never
// received stays in the registry and is revoked by the next RetireHost.
func (s *Signer) ReissueHost(signed []byte) (identity.Issued, error) {
	var req identity.ReissueHostRequest
	who, _, err := s.authorise(signed, identity.ActionReissueHost, &req, identity.RoleRoot, identity.RoleOperator)
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
	if len(cur.hosts[req.HostID]) == 0 {
		return identity.Issued{}, fmt.Errorf("%w: %s; register the serial of an offline pair with register-host", ErrUnknownHost, req.HostID)
	}
	issued, err := issueLeaf(cur.host, s.now(), req.HostID, identity.RoleHost, csr.PublicKey, serverAndClient, &x509.CertificateRequest{DNSNames: []string{req.HostID}})
	if err != nil {
		return identity.Issued{}, err
	}
	next := cur.withHost(req.HostID, issued.Serial)
	if err := commitFiles(s.dir, []stagedFile{{issuedFile, next.marshalIssued()}, {hostsFile, next.marshalHosts()}}); err != nil {
		return identity.Issued{}, err
	}
	s.auth = next
	s.log.Info("issued a further host certificate for a rotation", "host_id", req.HostID, "serial", issued.Serial,
		"requested_by", who.Subject, "requested_by_serial", who.Serial)
	return issued, nil
}

// RetireHost puts every serial the signer holds for a host, except the one the request keeps, on the
// revocation list, and leaves the kept serial as the host's only one. The revocation list and the
// record of the host reach the directory as one commit. It does nothing for a host that already
// holds only the kept serial, so a retry after a failure is safe.
func (s *Signer) RetireHost(signed []byte) error {
	var req identity.RetireHostRequest
	who, _, err := s.authorise(signed, identity.ActionRetireHost, &req, identity.RoleRoot, identity.RoleOperator)
	if err != nil {
		return err
	}
	if !namePattern.MatchString(req.HostID) {
		return fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}
	n, ok := new(big.Int).SetString(req.Keep, 16)
	if !ok || n.Sign() <= 0 {
		return fmt.Errorf("%w: serial %q is not hexadecimal", ErrBadRequest, req.Keep)
	}
	keep := n.Text(16)

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	held := cur.hosts[req.HostID]
	if !slices.Contains(held, keep) {
		return fmt.Errorf("%w: serial %s is not one of %s's", ErrUnknownHost, keep, req.HostID)
	}
	if len(held) == 1 {
		return nil
	}
	revoke := slices.DeleteFunc(slices.Clone(held), func(serial string) bool { return serial == keep || cur.crl.Revokes(serial) })
	now := s.now()
	next := cur.withHosts(req.HostID, []string{keep})
	files := []stagedFile{{hostsFile, next.marshalHosts()}}
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
	s.log.Info("retired a host's earlier certificates", "host_id", req.HostID, "kept", keep, "revoked", revoke, "seq", crl.Seq,
		"retired_by", who.Subject, "retired_by_serial", who.Serial)
	return nil
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
