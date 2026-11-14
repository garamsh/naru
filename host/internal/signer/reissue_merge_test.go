package signer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

func TestReissueRoot_AHigherListThatLacksTheNonceDoesNotReplaceTheOneThatHasIt(t *testing.T) {
	e := newEnv(t)
	x, y := e.s, replica(t, e)
	var c clock
	op := e.issue(identity.RoleOperator, "alice")
	signed, _ := e.request(&c, freshNonce(t))
	_, err := x.ReissueRoot(signed)
	require.NoError(t, err)
	revokeOnY := func(serial string) {
		_, err := y.Revoke(e.signed(e.rootID, identity.ActionRevoke, identity.RevokeRequest{Serial: serial}))
		require.NoError(t, err)
	}
	revokeOnY(op.cert.SerialNumber.Text(16))
	revokeOnY(e.rootID.cert.SerialNumber.Text(16))
	serialsX, seqX := serialsOf(t, e, x)
	serialsY, seqY := serialsOf(t, e, y)
	require.Greater(t, seqY, seqX, "Y's list is the higher one")
	require.Subset(t, serialsY, serialsX, "and holds every serial of X's")
	require.Empty(t, noncesOf(t, e, y))

	pushList(t, x, y)

	assert.NotEmpty(t, noncesOf(t, e, x), "X keeps the nonce it took")
	_, err = x.ReissueRoot(signed)
	assert.ErrorIs(t, err, signer.ErrReplay)
	serialsX, _ = serialsOf(t, e, x)
	assert.Contains(t, serialsX, op.cert.SerialNumber.Text(16), "and took Y's revocation")
}
