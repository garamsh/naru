package inventory_test

import (
	"context"
	"crypto"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// readmitting is the fake signer with the host removed and admitted again before it revokes.
type readmitting struct {
	*orchtest.Signer
	before func()
}

func (r readmitting) RevokeHost(ctx context.Context, signed []byte) (identity.HostSerials, error) {
	r.before()
	return r.Signer.RevokeHost(ctx, signed)
}

func TestAbandon_AHostAdmittedAgainBetweenTheTwoWritesIsNotFoundAndKeepsItsRecord(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	store := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	admit := func() {
		p := inventory.Pending{HostID: "host-a", PublicKey: []byte("k"), FirstSeen: now}
		require.NoError(t, store.PutPending(ctx, p, nil, true))
		require.NoError(t, store.Admit(ctx, p, inventory.Host{ID: "host-a", PublicKey: p.PublicKey}, orchapi.Event{Time: now, Type: inventory.EventAdmitted, Subject: "host-a"}))
	}
	admit()
	signer := readmitting{Signer: c.Signer, before: func() {
		_, err := store.RemoveHost(ctx, "host-a", orchapi.Event{Time: now, Type: inventory.EventRemoved, Subject: "host-a"})
		require.NoError(t, err)
		admit()
	}}
	svc := inventory.NewService(store, nil, signer, c.Clock, inventory.Config{})
	operator := c.CA.Issue(t, "alice", identity.RoleOperator)
	signed, err := identity.SignRequest(identity.ActionRevokeHost, identity.HostSerialsRequest{HostID: "host-a"}, time.Now().Add(time.Minute), operator.TLS.PrivateKey.(crypto.Signer), operator.Cert)
	require.NoError(t, err)

	_, err = svc.Abandon(ctx, "alice", "host-a", orchapi.AbandonRequest{Fenced: "off", RevokeHost: signed})
	require.ErrorIs(t, err, inventory.ErrNotFound)
	h, err := store.GetHost(ctx, "host-a")
	require.NoError(t, err)
	assert.Nil(t, h.Abandonment, "the host admitted again is not abandoned by the outcome")
}
