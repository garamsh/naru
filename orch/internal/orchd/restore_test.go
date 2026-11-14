package orchd_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// source is a running orch with an admitted host, a network and a group, and the plain snapshot of it.
type source struct {
	ca    *orchtest.CA
	root  orchtest.Identity
	plain []byte
}

func takeSource(t *testing.T) source {
	t.Helper()
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	o := startOne(t, orchtest.Options{Sealer: snapshot.SealTo(rec.X25519.PublicKey())})
	root := o.c.CA.Issue(t, "root", identity.RoleRoot)

	h := newHost(t, "host-a")
	_, err = o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"})
	require.NoError(t, err)
	_, err = o.op.ApplyNetwork(t.Context(), "lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24"})
	require.NoError(t, err)
	_, err = o.op.ApplyGroup(t.Context(), webGroup())
	require.NoError(t, err)

	sealed, err := o.c.Client(t, 0, &root).LatestSnapshot(t.Context())
	require.Error(t, err, "control: no snapshot has been taken yet")
	require.Nil(t, sealed)
	_, err = o.c.Client(t, 0, &root).Snapshot(t.Context())
	require.NoError(t, err)
	sealed, err = o.c.Client(t, 0, &root).LatestSnapshot(t.Context())
	require.NoError(t, err)
	plain, err := identity.Open(rec.X25519, sealed, []byte(snapshot.Info))
	require.NoError(t, err)
	return source{ca: o.c.CA, root: root, plain: plain}
}

// waiting starts a member of a new cluster that waits for a restore.
func (s source) waiting(t *testing.T) *orchtest.Cluster {
	t.Helper()
	return orchtest.Start(t, orchtest.Options{CA: s.ca, Restore: true})
}

func TestRestoreBringsBackWhatTheSnapshotHeldAndTheClusterGrowsBackToThree(t *testing.T) {
	src := takeSource(t)
	c := src.waiting(t)
	root := c.Client(t, 0, &src.root)

	// Until the restore the member serves nothing else.
	_, err := root.Groups(t.Context())
	require.Equal(t, http.StatusServiceUnavailable, status(err))
	require.Nil(t, c.Members[0].Etcd(), "no etcd runs before the restore")

	require.NoError(t, root.Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain)))

	groups, err := root.Groups(t.Context())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "web", groups[0].Spec.Name)
	nets, err := root.Networks(t.Context())
	require.NoError(t, err)
	require.Len(t, nets, 1)
	require.Equal(t, "lab", nets[0].Name)
	hosts, err := root.Hosts(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	require.Equal(t, "host-a", hosts[0].ID)

	// The other two members join the restored one as new members.
	c.AddMember(t)
	c.AddMember(t)
	require.Eventually(t, func() bool {
		l, err := c.Members[0].Etcd().MemberList(t.Context())
		return err == nil && len(l.Members) == 3
	}, 30*time.Second, 100*time.Millisecond)
	for i := 1; i <= 2; i++ {
		require.Eventually(t, func() bool {
			l, err := c.Client(t, i, &src.root).Groups(t.Context())
			return err == nil && len(l) == 1
		}, 30*time.Second, 100*time.Millisecond, "member %d holds the restored groups", i)
	}
}

func TestRestoreIsRootOnly(t *testing.T) {
	src := takeSource(t)
	c := src.waiting(t)
	operator := c.CA.Issue(t, "alice", identity.RoleOperator)

	err := c.Client(t, 0, &operator).Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain))
	require.Equal(t, http.StatusForbidden, status(err), "an operator")
	err = c.Client(t, 0, nil).Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain))
	require.Equal(t, http.StatusUnauthorized, status(err), "no certificate")
	require.Nil(t, c.Members[0].Etcd(), "nothing was restored")

	require.NoError(t, c.Client(t, 0, &src.root).Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain)), "control: root")
}

func TestRestoreRefusesASnapshotWhoseHashIsNotTheOneSent(t *testing.T) {
	src := takeSource(t)
	c := src.waiting(t)
	root := c.Client(t, 0, &src.root)

	err := root.Restore(t.Context(), bytes.NewReader(src.plain), sumHex([]byte("other")))
	require.Equal(t, http.StatusBadRequest, status(err), "a hash of other bytes")
	err = root.Restore(t.Context(), bytes.NewReader(src.plain), "abc")
	require.Equal(t, http.StatusBadRequest, status(err), "a hash that is not 64 digits")
	require.Nil(t, c.Members[0].Etcd(), "nothing was restored")

	// etcd's own check: the hash matches the bytes sent, but the snapshot is damaged.
	damaged := append([]byte(nil), src.plain...)
	damaged[len(damaged)/2] ^= 0xff
	err = root.Restore(t.Context(), bytes.NewReader(damaged), sumHex(damaged))
	require.Equal(t, http.StatusBadRequest, status(err), "a damaged snapshot with its own hash")
	truncated := src.plain[:len(src.plain)-sha256.Size]
	err = root.Restore(t.Context(), bytes.NewReader(truncated), sumHex(truncated))
	require.Equal(t, http.StatusBadRequest, status(err), "a snapshot without etcd's trailing hash")
	require.Nil(t, c.Members[0].Etcd(), "nothing was restored")

	// Control: the refusals left the member waiting, and the right hash restores.
	require.NoError(t, root.Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain)))
}

func TestASecondRestoreIsRefusedAndChangesNothing(t *testing.T) {
	src := takeSource(t)
	c := src.waiting(t)
	root := c.Client(t, 0, &src.root)
	require.NoError(t, root.Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain)))

	_, err := root.ApplyNetwork(t.Context(), "after", orchapi.NetworkSpec{Prefix: "10.8.0.0/24"})
	require.NoError(t, err)
	err = root.Restore(t.Context(), bytes.NewReader(src.plain), sumHex(src.plain))
	require.Equal(t, http.StatusConflict, status(err))
	_, err = root.Network(t.Context(), "after")
	require.NoError(t, err, "what was written after the restore is still there")
}

func TestRestoreFlagIsIgnoredWhenTheDataDirectoryHoldsAMember(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	root := c.CA.Issue(t, "root", identity.RoleRoot)
	cl := c.Client(t, 0, &root)
	_, err := cl.ApplyNetwork(t.Context(), "lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24"})
	require.NoError(t, err)

	// A restart of the same member with --restore runs etcd as usual and serves the whole API.
	again := c.RestartRestoring(t, 0)
	require.NotNil(t, again.Etcd(), "etcd runs")
	cl = c.Client(t, 0, &root)
	n, err := cl.Network(t.Context(), "lab")
	require.NoError(t, err)
	require.Equal(t, "lab", n.Name)
	require.Equal(t, http.StatusConflict, status(cl.Restore(t.Context(), bytes.NewReader([]byte("x")), sumHex([]byte("x")))))
}

func TestLatestSnapshotIsRootOnlyAndTheSealedBytes(t *testing.T) {
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	o := startOne(t, orchtest.Options{Sealer: snapshot.SealTo(rec.X25519.PublicKey())})
	root := o.c.CA.Issue(t, "root", identity.RoleRoot)
	cl := o.c.Client(t, 0, &root)

	_, err = o.op.LatestSnapshot(t.Context())
	require.Equal(t, http.StatusForbidden, status(err), "an operator")
	_, err = cl.LatestSnapshot(t.Context())
	require.Equal(t, http.StatusNotFound, status(err), "none taken yet")

	res, err := cl.Snapshot(t.Context())
	require.NoError(t, err)
	b, err := cl.LatestSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, int(res.Size), len(b))
	plain, err := identity.Open(rec.X25519, b, []byte(snapshot.Info))
	require.NoError(t, err, "the recovery key opens what was fetched")
	require.Contains(t, string(plain), "/naru/orch/")
	require.NotContains(t, string(b), "/naru/orch/", "the fetched bytes are sealed")
}
