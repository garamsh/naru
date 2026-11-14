package identity_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func TestRequest(t *testing.T) {
	c := newCA(t)
	root, rootKey := c.issue(t, "root", 10, now.Add(time.Hour), "root")
	other := newCA(t)
	foreign, foreignKey := other.issue(t, "root", 11, now.Add(time.Hour), "root")
	body := identity.RevokeRequest{Serial: "abc"}

	good, err := identity.SignRequest(identity.ActionRevoke, body, now.Add(time.Minute), rootKey, root)
	require.NoError(t, err)

	t.Run("a request for the action verifies and carries its body", func(t *testing.T) {
		id, raw, err := identity.VerifyRequest(good, identity.ActionRevoke, c.pool, nil, now)
		require.NoError(t, err)
		assert.Equal(t, identity.RoleRoot, id.Role)
		var got identity.RevokeRequest
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, body, got)
	})
	t.Run("another action is refused", func(t *testing.T) {
		_, _, err := identity.VerifyRequest(good, identity.ActionBundle, c.pool, nil, now)
		assert.ErrorIs(t, err, identity.ErrRequestAction)
	})
	t.Run("an expired request is refused", func(t *testing.T) {
		_, _, err := identity.VerifyRequest(good, identity.ActionRevoke, c.pool, nil, now.Add(time.Minute))
		assert.ErrorIs(t, err, identity.ErrRequestExpired)
	})
	t.Run("a revoked signer is refused", func(t *testing.T) {
		crl := &identity.CRL{Serials: []string{identity.SerialOf(root)}}
		_, _, err := identity.VerifyRequest(good, identity.ActionRevoke, c.pool, crl, now)
		assert.ErrorIs(t, err, identity.ErrRevoked)
	})
	t.Run("a certificate of another authority is refused", func(t *testing.T) {
		data, err := identity.SignRequest(identity.ActionRevoke, body, now.Add(time.Minute), foreignKey, foreign)
		require.NoError(t, err)
		_, _, err = identity.VerifyRequest(data, identity.ActionRevoke, c.pool, nil, now)
		assert.Error(t, err)
		_, _, err = identity.VerifyRequest(data, identity.ActionRevoke, other.pool, nil, now)
		assert.NoError(t, err, "control: the same request verifies against its own authority")
	})
	t.Run("a changed body is refused", func(t *testing.T) {
		var s map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(good, &s))
		var req map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(s["request"], &req))
		req["body"] = json.RawMessage(`{"serial":"def"}`)
		s["request"], err = json.Marshal(req)
		require.NoError(t, err)
		tampered, err := json.Marshal(s)
		require.NoError(t, err)
		_, _, err = identity.VerifyRequest(tampered, identity.ActionRevoke, c.pool, nil, now)
		assert.ErrorIs(t, err, identity.ErrRequestSignature)
	})
}
