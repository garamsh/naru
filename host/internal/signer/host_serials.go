package signer

import (
	"fmt"
	"slices"

	"github.com/garamsh/naru/lib/identity"
)

// LookupHost returns every serial the signer holds for the host a request root or an operator signed
// names, each with whether the revocation list carries it.
func (s *Signer) LookupHost(signed []byte) (identity.HostSerials, error) {
	var req identity.HostSerialsRequest
	if _, _, err := s.authorise(signed, identity.ActionLookupHost, &req, identity.RoleRoot, identity.RoleOperator); err != nil {
		return identity.HostSerials{}, err
	}
	if !namePattern.MatchString(req.HostID) {
		return identity.HostSerials{}, fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth.hostSerials(req.HostID)
}

// RevokeHost puts every serial the signer holds for the host a request root or an operator signed
// names on the revocation list, in one signing, and returns the serials as the list then stands. The
// host keeps its record, so a serial already revoked, or a retry, changes nothing.
func (s *Signer) RevokeHost(signed []byte) (identity.HostSerials, error) {
	var req identity.HostSerialsRequest
	who, _, err := s.authorise(signed, identity.ActionRevokeHost, &req, identity.RoleRoot, identity.RoleOperator)
	if err != nil {
		return identity.HostSerials{}, err
	}
	if !namePattern.MatchString(req.HostID) {
		return identity.HostSerials{}, fmt.Errorf("%w: host ID %q", ErrBadRequest, req.HostID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	held, err := cur.hostSerials(req.HostID)
	if err != nil {
		return identity.HostSerials{}, err
	}
	var revoke []string
	for _, h := range held.Serials {
		if !h.Revoked {
			revoke = append(revoke, h.Serial)
		}
	}
	if len(revoke) == 0 {
		return held, nil
	}
	next := *cur
	crl := cur.successor(s.now())
	crl.Serials = slices.Sorted(slices.Values(slices.Concat(crl.Serials, revoke)))
	if err := next.signCRL(crl); err != nil {
		return identity.HostSerials{}, err
	}
	if err := commitFiles(s.dir, []stagedFile{{crlFile, next.crlSigned}}); err != nil {
		return identity.HostSerials{}, err
	}
	s.auth = &next
	s.log.Info("revoked a host's certificates", "host_id", req.HostID, "revoked", revoke, "seq", crl.Seq,
		"revoked_by", who.Subject, "revoked_by_serial", who.Serial)
	return next.hostSerials(req.HostID)
}

// hostSerials lists the serials held for hostID, refusing a host with none.
func (a *authorities) hostSerials(hostID string) (identity.HostSerials, error) {
	held := a.hosts[hostID]
	if len(held) == 0 {
		return identity.HostSerials{}, fmt.Errorf("%w: %s; register the serial of its offline pair with naructl signer register-host", ErrUnknownHost, hostID)
	}
	out := identity.HostSerials{HostID: hostID, Serials: make([]identity.HostSerial, 0, len(held))}
	for _, serial := range held {
		out.Serials = append(out.Serials, identity.HostSerial{Serial: serial, Revoked: a.crl.Revokes(serial)})
	}
	return out, nil
}
