package signer

import (
	"context"
	"time"
)

// AuthorityCheckInterval is how often a serving signer asks whether an authority's certificate is
// due to be signed again.
const AuthorityCheckInterval = 24 * time.Hour

// RenewAuthorities signs again, under the same key, each authority whose certificate has less than
// identity.AuthorityRenewYears left, and keeps the new certificates in the directory (ADR 0121).
// GET /v1/ca, the bundle and a replica carry them from then on. It returns the names of the
// authorities it signed again; a blank signer has none.
func (s *Signer) RenewAuthorities() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth == nil {
		return nil, nil
	}
	next, names, err := s.auth.renewed(s.now())
	if err != nil || len(names) == 0 {
		return nil, err
	}
	if err := next.save(s.dir); err != nil {
		return nil, err
	}
	s.auth = next
	for _, name := range names {
		ca := map[string]authority{"host": next.host, "operator": next.operator, "orch": next.orch}[name]
		s.log.Warn("signed an authority's certificate again under its key: every pool that trusts it takes the new one from GET /v1/ca",
			"authority", name, "not_after", ca.cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return names, nil
}

// KeepAuthorities calls RenewAuthorities now and every AuthorityCheckInterval until ctx ends,
// logging a failure, which the next check tries again.
func (s *Signer) KeepAuthorities(ctx context.Context) {
	for {
		if _, err := s.RenewAuthorities(); err != nil {
			s.log.Error("cannot sign an authority's certificate again", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(AuthorityCheckInterval):
		}
	}
}
