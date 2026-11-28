package orchd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
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

// body is the message of orch's answer to a failed call.
func body(t *testing.T, err error) string {
	t.Helper()
	var e *orchapi.Error
	require.True(t, errors.As(err, &e), "not an answer of orch: %v", err)
	return e.Message
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// storedKey reads the enrollment key id as orch keeps it.
func storedKey(t *testing.T, o *one, id string) inventory.Key {
	t.Helper()
	k, err := invetcd.New(o.c.Members[0].Etcd()).GetKey(t.Context(), id)
	require.NoError(t, err)
	return k
}

func refusedEvents(t *testing.T, o *one, keyID string) []orchapi.Event {
	t.Helper()
	events, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	var out []orchapi.Event
	for _, ev := range events {
		if ev.Type == inventory.EventKeyRefused && ev.Subject == keyID {
			out = append(out, ev)
		}
	}
	return out
}

func TestEightHostsPresentingOneFreshKeyAtOnceLeaveOnePendingAndTheRestRefused(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	k, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Hour))
	require.NoError(t, err)

	const n = 8
	hosts := make([]host, n)
	for i := range hosts {
		hosts[i] = newHost(t, fmt.Sprintf("host-%d", i))
	}
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range hosts {
		wg.Go(func() {
			<-start
			_, errs[i] = o.anon.Enroll(t.Context(), hosts[i].request(k.Secret))
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatalf("a second host enrolled: %s and %s", hosts[winner].id, hosts[i].id)
			}
			winner = i
			continue
		}
		require.Equal(t, http.StatusForbidden, status(err), "%s: %v", hosts[i].id, err)
		assert.Equal(t, inventory.ErrKeyRefused.Error(), body(t, err), "a host that lost the race is told only that the key is refused")
	}
	require.NotEqual(t, -1, winner, "one host enrolls")
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, hosts[winner].id, pending[0].HostID)
	stored := storedKey(t, o, k.ID)
	assert.Equal(t, hosts[winner].id, stored.Host, "the key is bound to the host that is pending")
	assert.Equal(t, hosts[winner].pub, stored.HostKey)
}

func TestEveryRefusalOfAKeyGetsTheBodyAnUnknownKeyGetsAndAStoredKeysIsAnEvent(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	a, b, c := newHost(t, "host-a"), newHost(t, "host-b"), newHost(t, "host-c")
	_, err := o.anon.Enroll(t.Context(), a.request("naru-ek-"+fmt.Sprintf("%064x", 1)))
	require.Equal(t, http.StatusForbidden, status(err))
	unknown := body(t, err)
	assert.Equal(t, inventory.ErrKeyRefused.Error(), unknown)

	ka := o.keyOf(t, a.id)
	_, err = o.anon.Enroll(t.Context(), a.request(ka))
	require.NoError(t, err, "control: the key enrolls its first host")
	_, err = o.anon.Enroll(t.Context(), b.request(ka))
	require.Equal(t, http.StatusForbidden, status(err))
	assert.Equal(t, unknown, body(t, err), "a key bound to another host")

	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: a.id, Code: identity.PairingCode(a.pub), Statement: orchtest.Statement(t, o.operator, a.id, a.pub)})
	require.NoError(t, err)
	_, err = o.anon.Enroll(t.Context(), a.request(ka))
	require.Equal(t, http.StatusForbidden, status(err))
	assert.Equal(t, unknown, body(t, err), "a spent key, presented by its own host")
	_, err = o.anon.Enroll(t.Context(), c.request(ka))
	require.Equal(t, http.StatusForbidden, status(err))
	assert.Equal(t, unknown, body(t, err), "a spent key, presented by another host")

	keyA := storedKey(t, o, sha256Hex(ka)[:16])
	events := refusedEvents(t, o, keyA.ID)
	require.Len(t, events, 3, "each refusal of a stored key is an event naming it")
	assert.Equal(t, b.id, events[0].Detail["hostId"])
	assert.Contains(t, events[0].Detail["reason"], "another host")
	assert.Contains(t, events[1].Detail["reason"], "spent")
	all, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	for _, ev := range all {
		assert.False(t, ev.Type == inventory.EventKeyRefused && ev.Detail["hostId"] == a.id && ev.Subject != keyA.ID,
			"an unknown key names no stored key and is no event: %+v", ev)
	}
}

func TestAKeyMadeForLongerThanADayLapsesADayAfterItWasMade(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	s := invetcd.New(o.c.Members[0].Etcd())
	now := o.c.Clock.Now()
	put := func(secret string, created, expires time.Time) {
		h := sha256Hex(secret)
		require.NoError(t, s.PutKey(t.Context(), inventory.Key{ID: h[:16], Hash: h, CreatedAt: created, ExpiresAt: expires}))
	}
	old, fresh := "naru-ek-"+fmt.Sprintf("%064x", 7), "naru-ek-"+fmt.Sprintf("%064x", 8)
	put(old, now.Add(-25*time.Hour), now.AddDate(0, 0, 300))
	put(fresh, now.Add(-time.Hour), now.AddDate(0, 0, 300))

	_, err := o.anon.Enroll(t.Context(), newHost(t, "host-a").request(fresh))
	require.NoError(t, err, "control: a key made an hour ago enrolls, whatever expiry it was stored with")
	_, err = o.anon.Enroll(t.Context(), newHost(t, "host-b").request(old))
	require.Equal(t, http.StatusForbidden, status(err), "a key made before the upgrade with a life of 300 days lapsed a day after it was made")
}

func TestAFreshKeyAnAdmittedHostPresentsIsSpentWithIt(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	a, b := newHost(t, "host-a"), newHost(t, "host-b")
	_, err := o.anon.Enroll(t.Context(), a.request(o.keyOf(t, a.id)))
	require.NoError(t, err)
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: a.id, Code: identity.PairingCode(a.pub), Statement: orchtest.Statement(t, o.operator, a.id, a.pub)})
	require.NoError(t, err)

	k, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Hour))
	require.NoError(t, err)
	st, err := o.anon.Enroll(t.Context(), a.request(k.Secret))
	require.NoError(t, err)
	assert.Equal(t, orchapi.StateAdmitted, st.State, "the admitted host is answered admitted")
	stored := storedKey(t, o, k.ID)
	assert.Equal(t, []any{a.id, true}, []any{stored.Host, stored.Spent}, "and the key it presented is spent with it")
	_, err = o.anon.Enroll(t.Context(), b.request(k.Secret))
	require.Equal(t, http.StatusForbidden, status(err), "so it enrolls no other host")
}
