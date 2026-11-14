package signer

import (
	"crypto/ed25519"
	"fmt"
	"maps"
	"math/big"
	"path/filepath"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// Authorities returns the certificates of the three authorities and the revocation key's public
// half, which hold nothing secret.
func (s *Signer) Authorities() (identity.Authorities, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Authorities{}, err
	}
	return identity.Authorities{
		Host:          a.host.cert.Raw,
		Operator:      a.operator.cert.Raw,
		Orch:          a.orch.cert.Raw,
		RevocationKey: a.revocation.Public().(ed25519.PublicKey),
	}, nil
}

// CRL returns the revocation list, signed.
func (s *Signer) CRL() ([]byte, error) {
	a, err := s.ready()
	if err != nil {
		return nil, err
	}
	return a.crlSigned, nil
}

// Revoke adds the serial of a request root or an operator signed to the revocation list, signs the
// list again with the next sequence number, and returns it. A serial already on the list leaves it as it is.
func (s *Signer) Revoke(signed []byte) ([]byte, error) {
	var req identity.RevokeRequest
	who, _, err := s.authorise(signed, identity.ActionRevoke, &req, identity.RoleRoot, identity.RoleOperator)
	if err != nil {
		return nil, err
	}
	n, ok := new(big.Int).SetString(req.Serial, 16)
	if !ok || n.Sign() <= 0 {
		return nil, fmt.Errorf("%w: serial %q is not hexadecimal", ErrBadRequest, req.Serial)
	}
	serial := n.Text(16)

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	if cur.issued[serial] == string(identity.RoleRoot) && who.Role != identity.RoleRoot {
		return nil, fmt.Errorf("%w: %s may not revoke root's certificate", ErrForbidden, who.Role)
	}
	if slices.Contains(cur.crl.Serials, serial) {
		return cur.crlSigned, nil
	}
	next := *cur
	crl := cur.successor(s.now())
	crl.Serials = append(crl.Serials, serial)
	slices.Sort(crl.Serials)
	if err := next.signCRL(crl); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(s.dir, crlFile), next.crlSigned); err != nil {
		return nil, err
	}
	s.auth = &next
	s.log.Info("revoked a certificate", "serial", serial, "seq", crl.Seq, "revoked_by", who.Subject, "revoked_by_serial", who.Serial)
	return next.crlSigned, nil
}

// PushCRL takes a revocation list another signer holds, which the revocation key must have
// signed, and returns the list this signer holds afterwards. A list that adds no serial and no
// live nonce to its own changes nothing. A list with a higher sequence that holds all of both is
// adopted. Lists that each hold one the other lacks are merged and signed again at the higher
// sequence plus one. Taking a list is also how the signer learns its peers are reaching it.
func (s *Signer) PushCRL(data []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	if cur == nil {
		return nil, ErrNotInitialised
	}
	in, err := identity.VerifyCRL(cur.revocation.Public().(ed25519.PublicKey), data)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.notePush(now); err != nil {
		return nil, err
	}
	serials := make(map[string]struct{}, len(cur.crl.Serials)+len(in.Serials))
	for _, serial := range cur.crl.Serials {
		serials[serial] = struct{}{}
	}
	ownSerials := len(serials)
	inSerials := make(map[string]struct{}, len(in.Serials))
	for _, serial := range in.Serials {
		serials[serial] = struct{}{}
		inSerials[serial] = struct{}{}
	}
	nonces, inNonces := liveNonces(cur.crl.Nonces, now), liveNonces(in.Nonces, now)
	ownNonces := len(nonces)
	maps.Copy(nonces, inNonces)
	if len(serials) == ownSerials && len(nonces) == ownNonces {
		return cur.crlSigned, nil
	}
	next := *cur
	if len(serials) == len(inSerials) && len(nonces) == len(inNonces) && in.Seq > cur.crl.Seq {
		next.crl, next.crlSigned = *in, data
	} else {
		crl := identity.CRL{Seq: max(cur.crl.Seq, in.Seq) + 1, IssuedAt: now, Serials: slices.Sorted(maps.Keys(serials)), Nonces: nonces}
		if err := next.signCRL(crl); err != nil {
			return nil, err
		}
	}
	if err := writeFile(filepath.Join(s.dir, crlFile), next.crlSigned); err != nil {
		return nil, err
	}
	s.auth = &next
	s.log.Info("took a revocation list from a peer", "seq", next.crl.Seq, "serials", len(next.crl.Serials), "nonces", len(next.crl.Nonces), "pushed_seq", in.Seq)
	return next.crlSigned, nil
}

// successor returns the list that follows a's: the next sequence, the same serials, and the
// nonces whose requests have not expired at now, for the caller to add to before it signs.
func (a *authorities) successor(now time.Time) identity.CRL {
	return identity.CRL{Seq: a.crl.Seq + 1, IssuedAt: now, Serials: slices.Clone(a.crl.Serials), Nonces: liveNonces(a.crl.Nonces, now)}
}

// liveNonces returns a copy of nonces without those whose request has expired at now.
func liveNonces(nonces map[string]time.Time, now time.Time) map[string]time.Time {
	live := make(map[string]time.Time, len(nonces))
	for nonce, expiresAt := range nonces {
		if now.Before(expiresAt) {
			live[nonce] = expiresAt
		}
	}
	return live
}
