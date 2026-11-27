package orchd_test

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/machines"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// admitHost enrolls and admits a host of id, as an operator does.
func admitHost(t *testing.T, c *orchtest.Cluster, operator orchtest.Identity, id string) {
	t.Helper()
	admitted(t, c, operator, id)
}

// admitted is admitHost giving the host's own identity, from the certificate its poll fetches.
func admitted(t *testing.T, c *orchtest.Cluster, operator orchtest.Identity, id string) orchtest.Identity {
	t.Helper()
	op, anon := c.Client(t, 0, &operator), c.Client(t, 0, nil)
	k, err := op.CreateEnrollmentKey(t.Context(), c.Clock.Now().Add(time.Hour))
	require.NoError(t, err)
	h := newHost(t, id)
	_, err = anon.Enroll(t.Context(), h.request(k.Secret))
	require.NoError(t, err)
	_, err = op.Admit(t.Context(), orchapi.AdmitRequest{HostID: id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, operator, id, h.pub), Region: "kr-seoul"})
	require.NoError(t, err)
	st, err := anon.PollEnrollment(t.Context(), id, h.key, c.Clock.Now())
	require.NoError(t, err)
	block, _ := pem.Decode(st.Certificate)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return orchtest.Identity{TLS: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: h.key, Leaf: cert}, Cert: cert}
}

func TestOrch_AHeartbeatOfAnAbandonedHostIsGoneAndRecordedOnce(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	hostID := admitted(t, o.c, o.operator, "host-a")
	beat := o.c.Client(t, 0, &hostID)
	ctx := t.Context()
	require.NoError(t, beat.Heartbeat(ctx), "control: an admitted host reports")
	seen := hostByID(t, o.op, "host-a").LastSeen

	o.c.Signer.FailRevoke(errors.New("signer unreachable"))
	_, err := o.op.AbandonHost(ctx, "host-a", orchapi.AbandonRequest{Fenced: "powered off", RevokeHost: revokeHost(t, o.operator, identity.ActionRevokeHost, "host-a")})
	require.Error(t, err)
	o.c.Clock.Advance(time.Minute)
	for range 2 {
		err = beat.Heartbeat(ctx)
		assert.Equal(t, http.StatusGone, status(err), "the host's own log says it was abandoned")
	}
	assert.Equal(t, seen, hostByID(t, o.op, "host-a").LastSeen, "it is not seen as reporting")
	evs, err := o.op.Events(ctx)
	require.NoError(t, err)
	var beats []orchapi.Event
	for _, e := range evs {
		if e.Type == inventory.EventAbandonedHeartbeat {
			beats = append(beats, e)
		}
	}
	require.Len(t, beats, 1, "once, not on every heartbeat")
	assert.Equal(t, "host-a", beats[0].Subject)
	assert.Equal(t, "pending: the signer failed: signer unreachable", beats[0].Detail["revocation"])
}

// revokeHost is who's signed request to revoke the host of id, as naructl host abandon makes it.
func revokeHost(t *testing.T, who orchtest.Identity, action, id string) []byte {
	t.Helper()
	signed, err := identity.SignRequest(action, identity.HostSerialsRequest{HostID: id}, time.Now().Add(5*time.Minute), who.TLS.PrivateKey.(crypto.Signer), who.Cert)
	require.NoError(t, err)
	return signed
}

func hostByID(t *testing.T, cl *orchapi.Client, id string) orchapi.Host {
	t.Helper()
	hosts, err := cl.Hosts(t.Context())
	require.NoError(t, err)
	for _, h := range hosts {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("no host %s", id)
	return orchapi.Host{}
}

func TestOrch_AbandonsAFencedHostAndRecordsItsRevocation(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	admitHost(t, o.c, o.operator, "host-a")
	admitHost(t, o.c, o.operator, "host-b")
	ctx := t.Context()
	signed := revokeHost(t, o.operator, identity.ActionRevokeHost, "host-a")
	fenced := orchapi.AbandonRequest{Fenced: "powered off at the PDU", RevokeHost: signed}

	_, err := o.op.AbandonHost(ctx, "host-a", orchapi.AbandonRequest{Fenced: "  ", RevokeHost: signed})
	assert.Equal(t, http.StatusBadRequest, status(err), "no account of the fence")
	_, err = o.op.AbandonHost(ctx, "host-b", fenced)
	assert.Equal(t, http.StatusBadRequest, status(err), "a request to revoke another host")
	_, err = o.op.AbandonHost(ctx, "host-a", orchapi.AbandonRequest{Fenced: fenced.Fenced, RevokeHost: revokeHost(t, o.operator, identity.ActionLookupHost, "host-a")})
	assert.Equal(t, http.StatusBadRequest, status(err), "a request for another action")
	_, err = o.reader.AbandonHost(ctx, "host-a", fenced)
	assert.Equal(t, http.StatusForbidden, status(err), "a reader abandons nothing")
	automation := o.c.CA.Issue(t, "ci", identity.RoleAutomation)
	_, err = o.c.Client(t, 0, &automation).AbandonHost(ctx, "host-a", fenced)
	assert.Equal(t, http.StatusForbidden, status(err), "nor does automation: a person attests the fence")
	_, err = o.op.AbandonHost(ctx, "host-z", orchapi.AbandonRequest{Fenced: fenced.Fenced, RevokeHost: revokeHost(t, o.operator, identity.ActionRevokeHost, "host-z")})
	assert.Equal(t, http.StatusNotFound, status(err))
	assert.Nil(t, hostByID(t, o.op, "host-a").Abandonment, "nothing refused was recorded")
	assert.Empty(t, o.c.Signer.RevokedHosts(), "nor forwarded")

	o.c.Signer.FailRevoke(errors.New("signer unreachable"))
	_, err = o.op.AbandonHost(ctx, "host-a", fenced)
	require.Error(t, err, "the revocation failed")
	first := hostByID(t, o.op, "host-a").Abandonment
	require.NotNil(t, first, "the abandonment is recorded before the signer is asked")
	assert.Equal(t, "alice", first.By)
	assert.Equal(t, "powered off at the PDU", first.Fenced)
	assert.False(t, first.Revocation.Revoked())
	assert.Contains(t, first.Revocation.Error, "signer unreachable", "pending with the last error")

	o.c.Signer.FailRevoke(nil)
	o.c.Clock.Advance(time.Minute)
	got, err := o.op.AbandonHost(ctx, "host-a", orchapi.AbandonRequest{Fenced: "something else", RevokeHost: signed})
	require.NoError(t, err)
	require.NotNil(t, got.Abandonment)
	assert.Equal(t, first.At, got.Abandonment.At, "a repeat only retries the revocation")
	assert.Equal(t, "powered off at the PDU", got.Abandonment.Fenced)
	assert.True(t, got.Abandonment.Revocation.Revoked())
	assert.Equal(t, []string{"0ahost-a"}, got.Abandonment.Revocation.Serials)
	assert.Empty(t, got.Abandonment.Revocation.Error)
	assert.Equal(t, got.Abandonment, hostByID(t, o.op, "host-a").Abandonment, "the operator reads it on the host")
	assert.Equal(t, []string{"host-a"}, o.c.Signer.RevokedHosts())
	assert.Nil(t, hostByID(t, o.op, "host-b").Abandonment, "control: the other host is not abandoned")

	o.c.Signer.FailRevoke(errors.New("signer unreachable"))
	_, err = o.op.AbandonHost(ctx, "host-a", fenced)
	require.Error(t, err)
	assert.True(t, hostByID(t, o.op, "host-a").Abandonment.Revocation.Revoked(), "a revocation done stays recorded")

	evs, err := o.op.Events(ctx)
	require.NoError(t, err)
	var types []string
	for _, e := range evs {
		if e.Subject == "host-a" && e.Type != inventory.EventAdmitted && e.Type != inventory.EventEnrolling {
			types = append(types, e.Type)
		}
	}
	assert.Equal(t, []string{inventory.EventAbandoned, inventory.EventRevocationFailed, inventory.EventRevoked, inventory.EventRevocationFailed}, types)
}

func TestOrch_ARootIdentityAbandonsAHost(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	admitHost(t, o.c, o.operator, "host-a")
	root := o.c.CA.Issue(t, "root", identity.RoleRoot)
	got, err := o.c.Client(t, 0, &root).AbandonHost(t.Context(), "host-a", orchapi.AbandonRequest{Fenced: "decommissioned", RevokeHost: revokeHost(t, root, identity.ActionRevokeHost, "host-a")})
	require.NoError(t, err)
	assert.Equal(t, "root", got.Abandonment.By)
	assert.True(t, got.Abandonment.Revocation.Revoked())
}

// abandonedFrom is the Machine reconciler's hosts with each abandoned as the inventory records it,
// as hostclient.MachineHosts marks them.
type abandonedFrom struct {
	*orchtest.FakeMachineHosts
	inv inventory.Service
}

func (a abandonedFrom) List(ctx context.Context) ([]machines.Host, error) {
	hosts, err := a.FakeMachineHosts.List(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := a.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		for _, h := range inv {
			if h.ID == hosts[i].ID && h.Abandonment != nil {
				hosts[i].Abandoned = true
			}
		}
	}
	return hosts, nil
}

var _ machines.Hosts = abandonedFrom{}

func TestOrch_TheMachinesOfAnAbandonedHostAreLostAndTheirVolumesListed(t *testing.T) {
	hosts := orchtest.NewFakeMachineHosts(
		machines.Host{ID: "h-a", Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 64, Memory: 256 << 30, Disk: 4 << 40}},
		machines.Host{ID: "h-b", Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 64, Memory: 128 << 30, Disk: 4 << 40}},
	)
	o := startMachineOrchWith(t, orchtest.Options{NewMachineHosts: func(inv inventory.Service) machines.Hosts { return abandonedFrom{hosts, inv} }})
	ctx := t.Context()
	operator := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	reader := o.c.CA.Issue(t, "rita", identity.RoleReader)
	op, read := o.c.Client(t, 0, &operator), o.c.Client(t, 0, &reader)
	admitHost(t, o.c, operator, "h-a")
	m, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := read.Machine(ctx, "garam", "web")
		return err == nil && got.Status.Phase == machineapi.PhaseRunning
	}, 10*time.Second, 50*time.Millisecond)
	got, err := read.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.Equal(t, "h-a", got.Binding.Host)

	_, err = op.AbandonHost(ctx, "h-a", orchapi.AbandonRequest{Fenced: "powered off", RevokeHost: revokeHost(t, operator, identity.ActionRevokeHost, "h-a")})
	require.NoError(t, err)
	err = op.Remove(ctx, "h-a")
	assert.Equal(t, http.StatusConflict, orchStatus(t, err), "a Machine is still bound to it")
	require.Eventually(t, func() bool {
		got, err := o.garam.Machine(ctx, "web")
		return err == nil && got.Status.Phase == machineapi.PhaseLost
	}, 10*time.Second, 50*time.Millisecond, "the caller reads it lost")
	caller, err := o.garam.Machine(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, machines.ReasonHostLost, caller.Status.Reason)

	listed, err := read.AbandonedVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, orchapi.AbandonedVolumes{Caller: "garam", Name: "web", ID: m.ID, Host: "h-a", Epoch: 1, Volumes: []string{"data"}, At: listed[0].At, Role: orchapi.RoleBound}, listed[0])

	_, err = op.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{})
	assert.Equal(t, http.StatusConflict, orchStatus(t, err), "a lost Machine's volumes cannot move")
	err = read.DeleteAbandonedVolumes(ctx, "garam", "web", "h-a", 1)
	assert.Equal(t, http.StatusForbidden, orchStatus(t, err), "a reader deletes nothing")
	err = op.DeleteAbandonedVolumes(ctx, "garam", "web", "h-a", 2)
	assert.Equal(t, http.StatusNotFound, orchStatus(t, err))

	moved, err := op.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{AbandonVolumes: true})
	require.NoError(t, err)
	assert.True(t, moved.Move.AbandonVolumes)
	require.Eventually(t, func() bool {
		got, err := read.Machine(ctx, "garam", "web")
		return err == nil && got.Move == nil && got.Binding.Host == "h-b" && got.Status.Phase == machineapi.PhaseRunning
	}, 10*time.Second, 50*time.Millisecond, "it runs on h-b with new volumes")

	require.NoError(t, op.DeleteAbandonedVolumes(ctx, "garam", "web", "h-a", 1))
	listed, err = read.AbandonedVolumes(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed)
	require.NoError(t, op.Remove(ctx, "h-a"), "no Machine is bound to it any more")
}
