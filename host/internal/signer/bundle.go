package signer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/garamsh/naru/lib/identity"
)

// bundleInfo binds a bundle to the recovery key: it opens under no other purpose.
var bundleInfo = []byte("naru-signer bundle v1")

// Bundle returns every authority key sealed to the recovery key's X25519 public half, for a
// request root signed. Only the recovery key opens it.
func (s *Signer) Bundle(signed []byte) ([]byte, error) {
	var req struct{}
	who, a, err := s.authorise(signed, identity.ActionBundle, &req, identity.RoleRoot)
	if err != nil {
		return nil, err
	}
	sealed, err := sealBundle(a, a.recovery, bundleInfo)
	if err != nil {
		return nil, err
	}
	s.log.Info("produced the authority bundle", "requested_by", who.Subject, "requested_by_serial", who.Serial)
	return sealed, nil
}

func sealBundle(a *authorities, to identity.RecoveryPublic, info []byte) ([]byte, error) {
	key, err := to.SealKey()
	if err != nil {
		return nil, err
	}
	plain, err := a.marshalBundle()
	if err != nil {
		return nil, err
	}
	return identity.Seal(key, plain, info)
}

// Restore fills a blank directory from a sealed bundle. The recovery key opens the bundle, and
// root, whose certificate the bundle's own authorities must vouch for, authorises it, so the
// restored authorities are the ones the host certificates already issued chain to.
func (s *Signer) Restore(body identity.RestoreBody) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth != nil {
		return ErrInitialised
	}
	var req identity.RestoreRequest
	a, err := s.openBundleFor(body, &req)
	if err != nil {
		return err
	}
	if err := a.save(s.dir); err != nil {
		return err
	}
	s.auth = a
	s.log.Info("restored the authorities from a bundle", "bundle_sha256", req.BundleSHA256)
	return nil
}

// openBundleFor opens the bundle with the recovery key the body names, and checks that the
// authorisation is root's under the bundle's own authorities and names this bundle.
func (s *Signer) openBundleFor(body identity.RestoreBody, req *identity.RestoreRequest) (*authorities, error) {
	keyFile, err := readSmall(body.RecoveryKey)
	if err != nil {
		return nil, fmt.Errorf("%w: read the recovery key: %w", ErrBadRequest, err)
	}
	passphrase, err := readSmall(body.PassphraseFile)
	if err != nil {
		return nil, fmt.Errorf("%w: read the passphrase file: %w", ErrBadRequest, err)
	}
	recovery, err := identity.DecryptRecovery(strings.TrimRight(string(passphrase), "\r\n"), keyFile)
	if err != nil {
		return nil, err
	}
	plain, err := identity.Open(recovery.X25519, body.Bundle, bundleInfo)
	if err != nil {
		return nil, err
	}
	a, err := parseBundle(plain)
	if err != nil {
		return nil, err
	}
	who, raw, err := identity.VerifyRequest(body.Authorisation, identity.ActionRestore, a.operatorPool(), &a.crl, s.now())
	if err != nil {
		return nil, err
	}
	if who.Role != identity.RoleRoot {
		return nil, fmt.Errorf("%w: %s may not restore", ErrForbidden, who.Role)
	}
	if err := decode(raw, req); err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(body.Bundle); req.BundleSHA256 != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("%w: the request names another bundle", ErrBadRequest)
	}
	return a, nil
}

// maxSmallFile bounds a file the signer reads on a caller's word.
const maxSmallFile = 64 << 10

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a path the restore request names
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxSmallFile))
}
