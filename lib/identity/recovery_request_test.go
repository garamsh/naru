package identity_test

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func TestRecoveryRequest(t *testing.T) {
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	pub := rec.Ed25519.Public().(ed25519.PublicKey)
	other, err := identity.GenerateRecovery()
	require.NoError(t, err)
	body := identity.ReissueRootRequest{Nonce: "n", CSR: []byte("csr")}
	good, err := identity.SignRecoveryRequest(identity.ActionReissueRoot, body, now.Add(time.Minute), rec.Ed25519)
	require.NoError(t, err)

	t.Run("a request the key signed verifies and carries its body and expiry", func(t *testing.T) {
		raw, expires, err := identity.VerifyRecoveryRequest(good, identity.ActionReissueRoot, pub, now)
		require.NoError(t, err)
		var got identity.ReissueRootRequest
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, body, got)
		assert.True(t, expires.Equal(now.Add(time.Minute)))
	})
	t.Run("another key's signature is refused", func(t *testing.T) {
		_, _, err := identity.VerifyRecoveryRequest(good, identity.ActionReissueRoot, other.Ed25519.Public().(ed25519.PublicKey), now)
		assert.ErrorIs(t, err, identity.ErrRecoverySignature)
	})
	t.Run("another action is refused", func(t *testing.T) {
		_, _, err := identity.VerifyRecoveryRequest(good, identity.ActionRestore, pub, now)
		assert.ErrorIs(t, err, identity.ErrRequestAction)
	})
	t.Run("an expired request is refused", func(t *testing.T) {
		_, _, err := identity.VerifyRecoveryRequest(good, identity.ActionReissueRoot, pub, now.Add(time.Minute))
		assert.ErrorIs(t, err, identity.ErrRequestExpired)
	})
	t.Run("a changed body is refused", func(t *testing.T) {
		var s map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(good, &s))
		var req map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(s["request"], &req))
		req["body"] = json.RawMessage(`{"nonce":"m","csr":""}`)
		s["request"], err = json.Marshal(req)
		require.NoError(t, err)
		tampered, err := json.Marshal(s)
		require.NoError(t, err)
		_, _, err = identity.VerifyRecoveryRequest(tampered, identity.ActionReissueRoot, pub, now)
		assert.ErrorIs(t, err, identity.ErrRecoverySignature)
	})
	t.Run("nonces differ", func(t *testing.T) {
		a, err := identity.NewNonce()
		require.NoError(t, err)
		b, err := identity.NewNonce()
		require.NoError(t, err)
		assert.NotEqual(t, a, b)
		assert.Len(t, a, 2*identity.NonceBytes)
	})
}
