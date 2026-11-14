package crlsync_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/crlsync"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// memStore is a Store in memory. beforePut runs between a Get and the Put that follows, so a test
// can make another member store a list in between.
type memStore struct {
	mu        sync.Mutex
	data      []byte
	rev       int64
	puts      int
	beforePut func()
}

func (m *memStore) Get(context.Context) ([]byte, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data, m.rev, nil
}

func (m *memStore) Put(_ context.Context, data []byte, rev int64) (bool, error) {
	if m.beforePut != nil {
		f := m.beforePut
		m.beforePut = nil
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rev != m.rev {
		return false, nil
	}
	m.data, m.rev = data, m.rev+1
	m.puts++
	return true, nil
}

func (m *memStore) crl(t *testing.T, ca *orchtest.CA) *identity.CRL {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	crl, err := identity.VerifyCRL(ca.RevocationKey(), m.data)
	require.NoError(t, err)
	return crl
}

func syncer(ca *orchtest.CA, st crlsync.Store, p crlsync.Peer) *crlsync.Syncer {
	return &crlsync.Syncer{Store: st, Peer: p, Key: ca.RevocationKey(), Log: quiet}
}

func TestAnEmptyStoreTakesTheSignersList(t *testing.T) {
	ca := orchtest.NewCA(t)
	a := orchtest.NewCRLReplica(t, ca)
	st := &memStore{}
	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	assert.Equal(t, uint64(1), st.crl(t, ca).Seq)
}

func TestAHigherListOfTheSignerReplacesTheStoredOne(t *testing.T) {
	ca := orchtest.NewCA(t)
	a := orchtest.NewCRLReplica(t, ca)
	st := &memStore{}
	require.NoError(t, syncer(ca, st, a).Once(t.Context()))

	require.NoError(t, a.Revoke(ca.Issue(t, "alice", identity.RoleOperator)))
	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	assert.Equal(t, uint64(2), st.crl(t, ca).Seq)
	assert.Len(t, st.crl(t, ca).Serials, 1)
}

func TestTheStoredListIsPushedToTheSignerAndAMergeIsStored(t *testing.T) {
	ca := orchtest.NewCA(t)
	a, b := orchtest.NewCRLReplica(t, ca), orchtest.NewCRLReplica(t, ca)
	x, y := ca.Issue(t, "x", identity.RoleOperator), ca.Issue(t, "y", identity.RoleOperator)
	require.NoError(t, a.Revoke(x))
	require.NoError(t, b.Revoke(y))
	st := &memStore{}
	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	require.Equal(t, uint64(2), st.crl(t, ca).Seq)

	require.NoError(t, syncer(ca, st, b).Once(t.Context()))
	got := st.crl(t, ca)
	assert.ElementsMatch(t, []string{identity.SerialOf(x.Cert), identity.SerialOf(y.Cert)}, got.Serials)
	assert.Greater(t, got.Seq, uint64(2))
	assert.True(t, b.Has(x), "the stored list reached the signer")

	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	assert.True(t, a.Has(y), "and the other signer takes the merge at its next sync")
}

func TestALostCompareAndSwapIsNotAnErrorAndTheNextSyncConverges(t *testing.T) {
	ca := orchtest.NewCA(t)
	a, b := orchtest.NewCRLReplica(t, ca), orchtest.NewCRLReplica(t, ca)
	require.NoError(t, a.Revoke(ca.Issue(t, "x", identity.RoleOperator)))
	require.NoError(t, b.Revoke(ca.Issue(t, "y", identity.RoleOperator)))
	st := &memStore{}
	other := syncer(ca, st, b)
	st.beforePut = func() { require.NoError(t, other.Once(t.Context())) }

	require.NoError(t, syncer(ca, st, a).Once(t.Context()), "A's put loses to B's")
	assert.Equal(t, 1, st.puts, "only B's list was stored")
	assert.Len(t, st.crl(t, ca).Serials, 1)

	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	assert.Len(t, st.crl(t, ca).Serials, 2, "A read B's list, merged it and stored the merge")
}

func TestAListTheRevocationKeyDidNotSignIsNeitherPushedNorTrusted(t *testing.T) {
	ca := orchtest.NewCA(t)
	foreign, err := orchtest.NewCA(t).SignCRL(identity.CRL{Seq: 99, IssuedAt: time.Now(), Serials: []string{"1"}})
	require.NoError(t, err)
	a := orchtest.NewCRLReplica(t, ca)
	st := &memStore{data: foreign, rev: 1}

	require.NoError(t, syncer(ca, st, a).Once(t.Context()))
	crl := st.crl(t, ca)
	assert.Equal(t, uint64(1), crl.Seq, "the foreign list is replaced by the signer's")
	assert.Empty(t, crl.Serials)
}

// Three members of one orch, each with its own signer: a revoke on one signer reaches the others
// through etcd, and the operator is then refused on every member.
func TestARevokeOnOneSignerReachesTheOthersThroughEtcd(t *testing.T) {
	ca := orchtest.NewCA(t)
	replicas := make([]*orchtest.CRLReplica, 3)
	peers := make([]crlsync.Peer, 3)
	for i := range replicas {
		replicas[i] = orchtest.NewCRLReplica(t, ca)
		peers[i] = replicas[i]
	}
	orchtest.Start(t, orchtest.Options{CA: ca, CRLPeers: peers, CRLSyncInterval: 100 * time.Millisecond})
	op := ca.Issue(t, "alice", identity.RoleOperator)

	require.NoError(t, replicas[0].Revoke(op))
	assert.Eventually(t, func() bool { return replicas[1].Has(op) && replicas[2].Has(op) }, 20*time.Second, 50*time.Millisecond,
		"the signers B and C hold the serial revoked on A")
}
