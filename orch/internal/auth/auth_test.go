package auth_test

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// source serves the authority's list, or fails while down is set.
type source struct {
	ca   *orchtest.CA
	down bool
	data []byte
}

func (s *source) CRL(context.Context) ([]byte, error) {
	if s.down {
		return nil, errors.New("signer is down")
	}
	if s.data != nil {
		return s.data, nil
	}
	return s.ca.CRL()
}

func newAuth(src *source, clock func() time.Time) *auth.Revocations {
	return &auth.Revocations{Source: src, Key: src.ca.RevocationKey(), TTL: time.Minute, Now: clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func chains(id orchtest.Identity) [][]*x509.Certificate { return [][]*x509.Certificate{{id.Cert}} }

func TestARevokedSerialIsRefusedOnceTheListIsRefreshed(t *testing.T) {
	ca := orchtest.NewCA(t)
	now := time.Now()
	a := newAuth(&source{ca: ca}, func() time.Time { return now })
	op := ca.Issue(t, "alice", identity.RoleOperator)

	who, err := a.Authenticate(t.Context(), chains(op))
	require.NoError(t, err, "control: not yet revoked")
	require.Equal(t, identity.RoleOperator, who.Role)

	ca.Revoke(op)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.NoError(t, err, "the list is used until it is a minute old")
	now = now.Add(2 * time.Minute)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, identity.ErrRevoked)
}

func TestWithoutAnyListNobodyIsAuthenticated(t *testing.T) {
	ca := orchtest.NewCA(t)
	a := newAuth(&source{ca: ca, down: true}, time.Now)
	_, err := a.Authenticate(t.Context(), chains(ca.Issue(t, "alice", identity.RoleOperator)))
	require.ErrorIs(t, err, auth.ErrNoRevocationList)
}

func TestTheLastListIsKeptWhenARefreshFails(t *testing.T) {
	ca := orchtest.NewCA(t)
	op := ca.Issue(t, "alice", identity.RoleOperator)
	ca.Revoke(op)
	now := time.Now()
	src := &source{ca: ca}
	a := newAuth(src, func() time.Time { return now })
	_, err := a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, identity.ErrRevoked)

	src.down = true
	now = now.Add(5 * time.Minute)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, identity.ErrRevoked, "a signer outage does not un-revoke")
}

func TestAListThatCannotBeRefreshedIsRefusedOnceItIsOlderThanMaxAge(t *testing.T) {
	ca := orchtest.NewCA(t)
	op := ca.Issue(t, "alice", identity.RoleOperator)
	now := time.Now()
	src := &source{ca: ca}
	a := newAuth(src, func() time.Time { return now })
	_, err := a.Authenticate(t.Context(), chains(op))
	require.NoError(t, err)

	src.down = true
	now = now.Add(auth.DefaultMaxAge - time.Second)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.NoError(t, err, "control: still within the bound")
	now = now.Add(2 * time.Second)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, auth.ErrStaleRevocationList)

	src.down = false
	_, err = a.Authenticate(t.Context(), chains(op))
	require.NoError(t, err, "a refresh that succeeds ends the refusal")
}

func TestAReplayedOlderListIsNotTaken(t *testing.T) {
	ca := orchtest.NewCA(t)
	op := ca.Issue(t, "alice", identity.RoleOperator)
	old, err := ca.CRL() // seq 1, nothing revoked
	require.NoError(t, err)
	ca.Revoke(op)
	now := time.Now()
	src := &source{ca: ca}
	a := newAuth(src, func() time.Time { return now })
	_, err = a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, identity.ErrRevoked)

	src.data = old
	now = now.Add(5 * time.Minute)
	_, err = a.Authenticate(t.Context(), chains(op))
	require.ErrorIs(t, err, identity.ErrRevoked, "the replayed list omits the revocation and is refused")
}

func TestAListSignedByAnotherKeyIsNotTaken(t *testing.T) {
	ca, other := orchtest.NewCA(t), orchtest.NewCA(t)
	bad, err := other.CRL()
	require.NoError(t, err)
	a := newAuth(&source{ca: ca, data: bad}, time.Now)
	_, err = a.Authenticate(t.Context(), chains(ca.Issue(t, "alice", identity.RoleOperator)))
	require.ErrorIs(t, err, auth.ErrNoRevocationList)
}

func TestAllowedByRole(t *testing.T) {
	tests := []struct {
		role                              identity.Role
		read, operate, administer, report bool
	}{
		{identity.RoleRoot, true, true, true, false},
		{identity.RoleOperator, true, true, false, false},
		{identity.RoleAutomation, true, true, false, false},
		{identity.RoleReader, true, false, false, false},
		{identity.RoleHost, false, false, false, true},
		{identity.RoleOrch, false, false, false, false},
	}
	for _, tt := range tests {
		who := identity.Identity{Role: tt.role}
		require.Equal(t, tt.read, auth.Allowed(who, auth.Read), "%s read", tt.role)
		require.Equal(t, tt.operate, auth.Allowed(who, auth.Operate), "%s operate", tt.role)
		require.Equal(t, tt.administer, auth.Allowed(who, auth.Administer), "%s administer", tt.role)
		require.Equal(t, tt.report, auth.Allowed(who, auth.Report), "%s report", tt.role)
		require.True(t, auth.Allowed(who, auth.Any), "%s any", tt.role)
	}
	require.False(t, auth.Allowed(identity.Identity{}, auth.Any), "no role")
}
