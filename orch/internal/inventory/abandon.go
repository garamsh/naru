package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// maxFenced bounds the operator's account of how a host was fenced.
const maxFenced = 1024

func (s *service) Abandon(ctx context.Context, by, hostID string, req orchapi.AbandonRequest) (orchapi.Host, error) {
	if err := validHostID(hostID); err != nil {
		return orchapi.Host{}, err
	}
	fenced := strings.TrimSpace(req.Fenced)
	switch {
	case fenced == "":
		return orchapi.Host{}, invalid("fenced: say how the host was fenced; a host is abandoned only once it is off")
	case len(fenced) > maxFenced:
		return orchapi.Host{}, invalid("fenced: longer than %d bytes", maxFenced)
	}
	// Orch does not verify the request, the signer does; it reads it only to forward no request but
	// the revocation of this host.
	action, body, err := identity.ReadRequest(req.RevokeHost)
	if err != nil {
		return orchapi.Host{}, invalid("revokeHost: %v", err)
	}
	var target identity.HostSerialsRequest
	if err := json.Unmarshal(body, &target); err != nil {
		return orchapi.Host{}, invalid("revokeHost: %v", err)
	}
	if action != identity.ActionRevokeHost || target.HostID != hostID {
		return orchapi.Host{}, invalid("revokeHost: a request for %s of %q, not %s of %q", action, target.HostID, identity.ActionRevokeHost, hostID)
	}
	have, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return orchapi.Host{}, err
	}
	if have.Abandonment == nil {
		now := s.clock.Now().UTC()
		ev := orchapi.Event{Time: now, Type: EventAbandoned, Subject: hostID, Detail: map[string]string{"by": by, "fenced": fenced}}
		// The write is guarded on the record as read, so an abandonment another request made since is
		// kept as it was.
		if have, err = s.store.UpdateHost(ctx, hostID, func(h *Host) {
			if h.Abandonment == nil {
				h.Abandonment = &orchapi.HostAbandonment{By: by, At: now, Fenced: fenced}
			}
		}, &ev); err != nil {
			return orchapi.Host{}, err
		}
	}
	return s.revoke(ctx, by, have, req.RevokeHost)
}

// revoke forwards the signed request and records its outcome on the abandoned host's record. A
// revocation that succeeded once stays recorded whatever a repeat answers.
func (s *service) revoke(ctx context.Context, by string, h Host, signed []byte) (orchapi.Host, error) {
	got, err := s.signer.RevokeHost(ctx, signed)
	if err == nil && got.HostID != h.ID {
		err = fmt.Errorf("%w: the signer answered the serials of %q", ErrSignerFailed, got.HostID)
	}
	var revoked []string
	for _, sr := range got.Serials {
		if sr.Revoked {
			revoked = append(revoked, sr.Serial)
		}
	}
	if err == nil && len(revoked) == 0 {
		err = fmt.Errorf("%w: the signer holds no revoked serial for %s", ErrSignerFailed, h.ID)
	}
	now := s.clock.Now().UTC()
	ev := orchapi.Event{Time: now, Type: EventRevoked, Subject: h.ID, Detail: map[string]string{"by": by, "serials": strings.Join(revoked, ",")}}
	if err != nil {
		if !errors.Is(err, ErrSignerRefused) && !errors.Is(err, ErrSignerFailed) {
			err = fmt.Errorf("%w: %v", ErrSignerFailed, err)
		}
		ev = orchapi.Event{Time: now, Type: EventRevocationFailed, Subject: h.ID, Detail: map[string]string{"by": by, "error": err.Error()}}
	}
	out, werr := s.store.UpdateHost(ctx, h.ID, func(h *Host) {
		r := &h.Abandonment.Revocation
		switch {
		case err == nil:
			r.RevokedAt, r.Serials, r.Error = now, revoked, ""
		case !r.Revoked():
			r.Error = err.Error()
		}
	}, &ev)
	if werr != nil {
		return orchapi.Host{}, errors.Join(err, werr)
	}
	return out.api(), err
}
