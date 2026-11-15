package api

import (
	"fmt"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/orchapi"
)

// revocationTTL is how long the API uses a revocation list before it asks for a fresh one.
const revocationTTL = 30 * time.Second

// revocationSource returns where the Node API reads the signed revocation list from, per the
// Node's configuration: the signer on this Node, over socket, or else orch. It is nil when the Node
// has neither. The source is read once, when the API starts; a reload changes the key and the max
// age but not where the list comes from.
func revocationSource(spec config.Spec, socket string) (identity.CRLSource, error) {
	from, ok := spec.RevocationFrom()
	switch {
	case !ok:
		return nil, nil
	case from.Socket != "":
		return identity.SocketSource{Path: socket}, nil
	}
	return orchapi.RevocationSource(from.OrchURL, from.OrchCASHA256)
}

// newRevocations returns the list the API checks its clients against, and nil when a has no
// revocationPublicKey: the API then checks no revocation. A list with the same key and max age as
// prev is prev itself, so a reload keeps what it has fetched.
func (s *Server) newRevocations(a config.API, prev *identity.RevocationList) (*identity.RevocationList, error) {
	if a.RevocationPublicKey == "" {
		return nil, nil
	}
	key, err := identity.ParseRevocationPublic([]byte(a.RevocationPublicKey))
	if err != nil {
		return nil, fmt.Errorf("spec.api.revocationPublicKey: %w", err)
	}
	maxAge, err := config.RevocationMaxAgeOf(a.RevocationMaxAge)
	if err != nil {
		return nil, fmt.Errorf("spec.api.revocationMaxAge: %w", err)
	}
	if prev != nil && prev.Key.Equal(key) && prev.MaxAge == maxAge {
		return prev, nil
	}
	if s.crlSource == nil {
		return nil, fmt.Errorf("spec.api.revocationPublicKey: the Node runs no signer and has no spec.host.orch to read the list from")
	}
	return &identity.RevocationList{
		Source: s.crlSource, Key: key, TTL: revocationTTL, MaxAge: maxAge,
		File: s.deps.RevocationFile, Now: time.Now, Log: s.deps.Log,
	}, nil
}

// Revocations returns the list the API checks its clients against, and nil when it checks none.
func (s *Server) Revocations() *identity.RevocationList {
	return s.revocations.Load()
}
