package orchd_test

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const allocKeys = "/naru/orch/allocations/"

func (o *one) allocations(t *testing.T) map[string]inventory.Allocation {
	t.Helper()
	resp, err := o.c.Members[0].Etcd().Get(t.Context(), allocKeys, clientv3.WithPrefix())
	require.NoError(t, err)
	out := map[string]inventory.Allocation{}
	for _, kv := range resp.Kvs {
		var a inventory.Allocation
		require.NoError(t, json.Unmarshal(kv.Value, &a))
		out[a.Prefix] = a
	}
	return out
}

func (o *one) tryAdmit(t *testing.T, h host) (orchapi.Host, error) {
	return o.op.Admit(t.Context(), orchapi.AdmitRequest{
		HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul",
	})
}

func TestConcurrentAdmissionsAgainstASmallPoolGiveEachHostItsOwnPrefixAndIssueNothingForTheRest(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	// A /22 holds four /24s and gives up the first: three are left for five hosts.
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/22")))
	var hosts []host
	for _, id := range []string{"h-a", "h-b", "h-c", "h-d", "h-e"} {
		h := newHost(t, id)
		hosts = append(hosts, h)
		_, err := o.anon.Enroll(t.Context(), h.seeded(o.keyOf(t, h.id)))
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	var won, full atomic.Int32
	for _, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := o.op.Admit(t.Context(), orchapi.AdmitRequest{
				HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul",
			})
			switch {
			case err == nil:
				won.Add(1)
			case status(err) == http.StatusConflict:
				full.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(3), won.Load())
	assert.Equal(t, int32(2), full.Load())
	assert.Equal(t, 3, o.c.Signer.Calls(), "a certificate is issued only for a host that holds a prefix")

	listed, err := o.op.Hosts(t.Context())
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, h := range listed {
		require.False(t, seen[h.MachinePrefix], "prefix %s given twice", h.MachinePrefix)
		seen[h.MachinePrefix] = true
	}
	allocs := o.allocations(t)
	require.Len(t, allocs, 3)
	for _, a := range allocs {
		assert.True(t, a.ReservedUntil.IsZero(), "every allocation is confirmed")
	}
}

func TestASignerFailureDropsTheReservationAndTheNextAdmissionGetsTheSamePrefix(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
	h := newHost(t, "h-a")
	_, err := o.anon.Enroll(t.Context(), h.seeded(o.keyOf(t, h.id)))
	require.NoError(t, err)

	o.c.Signer.Fail(errors.New("the signer is down"))
	_, err = o.tryAdmit(t, h)
	assert.Equal(t, http.StatusBadGateway, status(err))
	assert.Empty(t, o.allocations(t), "the failed admission holds no prefix")

	o.c.Signer.Fail(nil)
	got, err := o.tryAdmit(t, h)
	require.NoError(t, err)
	assert.Equal(t, "10.200.1.0/24", got.MachinePrefix)
}

func (o *one) putReservation(t *testing.T, host, prefix string, until time.Time) {
	t.Helper()
	b, err := json.Marshal(inventory.Allocation{Prefix: prefix, HostID: host, ReservedUntil: until})
	require.NoError(t, err)
	_, err = o.c.Members[0].Etcd().Put(t.Context(), allocKeys+prefix, string(b))
	require.NoError(t, err)
}

func TestAReservationLeftByACrashedAdmissionIsTakenOverOnlyOnceItHasExpired(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
	now := o.c.Clock.Now()

	// An admission that is still inside its reservation holds the prefix.
	o.putReservation(t, "ghost", "10.200.1.0/24", now.Add(time.Minute))
	a := newHost(t, "h-a")
	_, err := o.anon.Enroll(t.Context(), a.seeded(o.keyOf(t, a.id)))
	require.NoError(t, err)
	got, err := o.tryAdmit(t, a)
	require.NoError(t, err)
	assert.Equal(t, "10.200.2.0/24", got.MachinePrefix, "a live reservation is skipped")

	// One that ran out is taken over, and the prefix is the lowest free one again.
	o.putReservation(t, "ghost", "10.200.1.0/24", now.Add(-time.Second))
	b := newHost(t, "h-b")
	_, err = o.anon.Enroll(t.Context(), b.seeded(o.keyOf(t, b.id)))
	require.NoError(t, err)
	got, err = o.tryAdmit(t, b)
	require.NoError(t, err)
	assert.Equal(t, "10.200.1.0/24", got.MachinePrefix, "an expired reservation is taken over")
	assert.Equal(t, "h-b", o.allocations(t)["10.200.1.0/24"].HostID)
}

func TestAReEnrollmentRacingTheAdmissionLeavesNoPendingRecordNextToTheHost(t *testing.T) {
	for range 5 {
		o := startOne(t, orchtest.Options{Limits: inventory.Config{SourceRate: 1000, SourceBurst: 1000, KeyRate: 1000, KeyBurst: 1000}})
		require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
		h := newHost(t, "h-a")
		key := o.keyOf(t, h.id)
		_, err := o.anon.Enroll(t.Context(), h.seeded(key))
		require.NoError(t, err)

		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 25 {
					// Once the host is admitted its key is spent, and a repeat is refused.
					if _, err := o.anon.Enroll(t.Context(), h.seeded(key)); err != nil && status(err) != http.StatusForbidden {
						t.Error(err)
						return
					}
				}
			}()
		}
		_, err = o.tryAdmit(t, h)
		require.NoError(t, err)
		wg.Wait()

		pending, err := o.op.Pending(t.Context())
		require.NoError(t, err)
		require.Empty(t, pending, "an admitted host is not pending again")
		_, err = o.anon.Enroll(t.Context(), h.seeded(key))
		require.Equal(t, http.StatusForbidden, status(err), "the admitted host's key is spent")
	}
}

func TestPollAnswersTheSame404ForAnUnknownHostAndABadProofAndLimitsASource(t *testing.T) {
	o := startOne(t, orchtest.Options{Limits: inventory.Config{PollRate: 0.001, PollBurst: 5}})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.keyOf(t, h.id)))
	require.NoError(t, err)

	st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err, "a pending host with a valid proof")
	assert.Equal(t, orchapi.StatePending, st.State)

	other := newHost(t, "host-a")
	_, badProof := o.anon.PollEnrollment(t.Context(), h.id, other.key, o.c.Clock.Now())
	_, stale := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now().Add(-time.Hour))
	_, unknown := o.anon.PollEnrollment(t.Context(), "host-zzz", other.key, o.c.Clock.Now())
	for name, err := range map[string]error{"bad proof": badProof, "stale proof": stale, "unknown host": unknown} {
		assert.Equal(t, http.StatusNotFound, status(err), name)
	}
	assert.Equal(t, badProof.Error(), unknown.Error(), "the bodies do not tell a known host from an unknown one")
	assert.Equal(t, stale.Error(), unknown.Error())

	// The burst is spent: the source is limited, whoever it polls for.
	_, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err, "control: the fifth poll is still inside the burst")
	_, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	assert.Equal(t, http.StatusTooManyRequests, status(err))
}

func TestABodyOverTheLimitAnswers413NotAMalformedRequest(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: o.c.CA.Pool()}}}
	post := func(body string) int {
		resp, err := hc.Post("https://"+o.c.Members[0].Addr()+"/v1/enroll", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	big := `{"hostId":"` + strings.Repeat("a", 2<<20) + `"}`
	assert.Equal(t, http.StatusRequestEntityTooLarge, post(big))
	assert.Equal(t, http.StatusBadRequest, post(`{"hostId":`), "control: a malformed body is still a 400")
}

func TestAStoredHostHoldsNoCredentialAndTheAnswerFollowsTheProfile(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	const first, second = "hunter2-first-credential", "hunter2-second-credential"
	reg := func(pw string) []byte {
		return profileYAML(t, "10.200.0.0/16", layout.Registry{Host: "registry.example:5000", Username: "robot", Password: pw})
	}
	require.NoError(t, o.op.SetProfile(t.Context(), reg(first)))
	h := newHost(t, "h-0123456789abcdef")
	o.admitSeeded(t, h)

	resp, err := o.c.Members[0].Etcd().Get(t.Context(), "/naru/orch/", clientv3.WithPrefix())
	require.NoError(t, err)
	for _, kv := range resp.Kvs {
		if string(kv.Key) == "/naru/orch/profile" {
			continue
		}
		assert.NotContains(t, string(kv.Value), first, "%s", kv.Key)
		assert.NotContains(t, string(kv.Value), "config", "%s", kv.Key)
	}

	st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	assert.Contains(t, string(st.Config), first)

	require.NoError(t, o.op.SetProfile(t.Context(), reg(second)))
	st, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	assert.Contains(t, string(st.Config), second, "a profile change shows in the next answer")
	assert.NotContains(t, string(st.Config), first)

	// Without a profile there is nothing to build from: the answer is a conflict, not a stale copy.
	_, err = o.c.Members[0].Etcd().Delete(t.Context(), "/naru/orch/profile")
	require.NoError(t, err)
	_, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	assert.Equal(t, http.StatusConflict, status(err))
}

func TestANewLeaderDropsTheConfigOlderHostRecordsCarry(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 3})
	all := []int{0, 1, 2}
	first := c.Leader(t, all, 30*time.Second)
	var live []int
	for _, i := range all {
		if i != first {
			live = append(live, i)
		}
	}
	kv := c.Members[live[0]].Etcd()
	for _, id := range []string{"h1", "h2"} {
		_, err := kv.Put(t.Context(), "/naru/orch/hosts/"+id, `{"id":"`+id+`","region":"r","machinePrefix":"10.200.1.0/24","config":"cGFzc3dvcmQ6IGh1bnRlcjI="}`)
		require.NoError(t, err)
	}
	require.NoError(t, c.Members[first].Stop())
	c.Leader(t, live, failover)

	require.Eventually(t, func() bool {
		resp, err := kv.Get(t.Context(), "/naru/orch/hosts/", clientv3.WithPrefix())
		if err != nil || len(resp.Kvs) != 2 {
			return false
		}
		for _, r := range resp.Kvs {
			if strings.Contains(string(r.Value), "config") {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond, "the new leader rewrites the records")
}
