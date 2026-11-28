package orchd_test

import (
	"crypto"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/machineapi/machineapitest"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/machines"
	machinesetcd "github.com/garamsh/naru/orch/internal/machines/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const machineZone = "m.example.test"

// machineSpec is a spec orch records, and runs on a host when it has one.
func machineSpec() machineapi.Spec {
	return machineapi.Spec{
		Image:         "registry.example.test/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CPU:           1,
		MemoryBytes:   512 << 20,
		RootDiskBytes: 1 << 30,
		Volumes:       []machineapi.Volume{{Name: "data", SizeBytes: 1 << 30, MountPath: "/data"}},
		Services:      []machineapi.Service{{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP}},
		Env:           map[string]string{"TOKEN": "s3cret"},
	}
}

// machineOrch is one orch member serving the Machine API, with two callers and an operator.
type machineOrch struct {
	c                    *orchtest.Cluster
	garamID              orchtest.Identity
	garam, functions, op *machineapi.Client
}

func startMachineOrch(t *testing.T) machineOrch {
	t.Helper()
	return startMachineOrchWith(t, orchtest.Options{})
}

// startMachineOrchWith starts the member with the options o adds, such as its hosts.
func startMachineOrchWith(t *testing.T, o orchtest.Options) machineOrch {
	t.Helper()
	o.Members, o.MachineZone = 1, machineZone
	c := orchtest.Start(t, o)
	garam := c.CA.Issue(t, "garam", identity.RoleCaller)
	return machineOrch{
		c: c, garamID: garam, garam: c.MachineClient(t, 0, garam),
		functions: c.MachineClient(t, 0, c.CA.Issue(t, "functions", identity.RoleCaller)),
		op:        c.MachineClient(t, 0, c.CA.Issue(t, "alice", identity.RoleOperator)),
	}
}

// oneHost is a host that fits every Machine the scenarios make, and holds each delete for longer
// than a round trip of the API.
func oneHost() *orchtest.FakeMachineHosts {
	h := orchtest.NewFakeMachineHosts(machines.Host{ID: "h-a", Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 64, Memory: 256 << 30, Disk: 4 << 40}})
	h.DeleteDelay = 300 * time.Millisecond
	return h
}

// aliasDomain is the domain outside the zone the operator grants garam.
const aliasDomain = "apps.garam.example"

func TestOrch_PassesTheMachineAPIConformanceScenarios(t *testing.T) {
	hosts := oneHost()
	// An hour between passes: every pass after the first is the one a write of a Machine or a
	// snapshot wakes, as the reconciler runs at once after an API write.
	o := startMachineOrchWith(t, orchtest.Options{NewMachineHosts: func(inventory.Service) machines.Hosts { return hosts }, MachineInterval: time.Hour})
	kv := o.c.Members[0].Etcd()
	alice := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	_, err := o.c.Client(t, 0, &alice).ApplyCaller(t.Context(), "garam", orchapi.CallerSpec{AliasDomains: []string{aliasDomain}})
	require.NoError(t, err)
	cfg := machineapitest.Config{
		AliasDomain: aliasDomain,
		Caller:      o.garam, Other: o.functions, OtherRole: o.op, Zone: machineZone, Spec: machineSpec(), Timeout: 10 * time.Second,
		Anonymous: o.c.MachineClient(t, 0, orchtest.Identity{}), Token: func(_ *testing.T, name string) string { return o.c.Signer.CallerToken(name) },
		CallerKey: o.garamID.TLS.PrivateKey.(crypto.Signer), CallerCert: o.garamID.Cert,
		// The log's age drops every event the caller's log holds: its trim deletes them and records
		// the floor as the newest it dropped.
		ExpireLog: func(t *testing.T) {
			del, err := kv.Delete(t.Context(), machinesetcd.EventPrefix+"garam/", clientv3.WithPrefix())
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), machinesetcd.FloorPrefix+"garam", strconv.FormatInt(del.Header.Revision, 10))
			require.NoError(t, err)
		},
	}
	machineapitest.Run(t, cfg)
}

// refused asserts err is the Machine API's refusal with status and code.
func refused(t *testing.T, err error, status int, code machineapi.Code, what string) {
	t.Helper()
	var e *machineapi.Error
	require.True(t, errors.As(err, &e), "%s: refused with a Machine API error, got %v", what, err)
	assert.Equal(t, status, e.Status, "%s (%s)", what, e.Message)
	assert.Equal(t, code, e.Code, "%s (%s)", what, e.Message)
}

func TestOrch_KeepsAMachineUnderItsCallerWithAnIDOfItsOwn(t *testing.T) {
	o := startMachineOrch(t)
	m, created, err := o.garam.CreateMachine(t.Context(), "web", machineSpec())
	require.NoError(t, err)
	require.True(t, created)
	assert.Regexp(t, `^[0-9a-f]{16}$`, m.ID)
	assert.Equal(t, machineapi.PhasePending, m.Status.Phase, "no host places it here")

	kv := o.c.Members[0].Etcd()
	resp, err := kv.Get(t.Context(), "/naru/orch/machines/garam/web")
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 1, "the record is under the caller and the name")
	var stored struct {
		ID   string          `json:"id"`
		Spec machineapi.Spec `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(resp.Kvs[0].Value, &stored))
	assert.Equal(t, m.ID, stored.ID)
	assert.Equal(t, "s3cret", stored.Spec.Env["TOKEN"], "orch keeps the value it compares and runs")
	assert.Equal(t, machineapi.Redacted, m.Spec.Env["TOKEN"], "and answers none")
	ids, err := kv.Get(t.Context(), "/naru/orch/machine-ids/"+m.ID)
	require.NoError(t, err)
	require.Len(t, ids.Kvs, 1, "the id is claimed")
	assert.Equal(t, "garam/web", string(ids.Kvs[0].Value))

	theirs, created, err := o.functions.CreateMachine(t.Context(), "web", machineSpec())
	require.NoError(t, err)
	assert.True(t, created, "the same name under another caller is another Machine")
	assert.NotEqual(t, m.ID, theirs.ID)
	all, err := kv.Get(t.Context(), "/naru/orch/machines/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	require.NoError(t, err)
	var keys []string
	for _, k := range all.Kvs {
		keys = append(keys, string(k.Key))
	}
	assert.Equal(t, []string{"/naru/orch/machines/functions/web", "/naru/orch/machines/garam/web"}, keys)
}

func TestOrch_AnswersAnotherCallersMachineAndSnapshotAsNotFound(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	_, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	_, _, err = o.garam.PutSnapshot(ctx, "before", machineapi.SnapshotRequest{Machine: "web"})
	require.NoError(t, err)

	for what, call := range map[string]func(cl *machineapi.Client) error{
		"read":          func(cl *machineapi.Client) error { _, err := cl.Machine(ctx, "web"); return err },
		"snapshot read": func(cl *machineapi.Client) error { _, err := cl.Snapshot(ctx, "before"); return err },
	} {
		require.NoError(t, call(o.garam), "the caller's own %s", what)
		refused(t, call(o.functions), http.StatusNotFound, machineapi.CodeNotFound, "another caller's "+what)
	}
	_, err = o.functions.StartMachine(ctx, "web")
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "another caller's start")
	_, err = o.functions.StopMachine(ctx, "web", machineapi.StopRequest{})
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "another caller's stop")
	refused(t, o.functions.DeleteMachine(ctx, "web"), http.StatusNotFound, machineapi.CodeNotFound, "another caller's delete")
	refused(t, o.functions.DeleteSnapshot(ctx, "before"), http.StatusNotFound, machineapi.CodeNotFound, "another caller's snapshot delete")
	_, _, err = o.functions.PutSnapshot(ctx, "theirs", machineapi.SnapshotRequest{Machine: "web"})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "a snapshot of another caller's Machine")
	restore := machineSpec()
	restore.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "before"}
	_, created, err := o.garam.CreateMachine(ctx, "restored", restore)
	require.NoError(t, err, "the caller's own restore")
	require.True(t, created)
	_, _, err = o.functions.CreateMachine(ctx, "restored", restore)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotNotFound, "a restore from another caller's snapshot")

	list, err := o.functions.Machines(ctx)
	require.NoError(t, err)
	assert.Empty(t, list.Machines, "another caller lists none of the caller's")
	page, err := o.functions.Events(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, page.Events, "nor reads its events")
	got, err := o.garam.Machine(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, machineapi.StateRunning, got.Spec.State, "no refused call changed the caller's Machine")
	assert.Equal(t, machineapi.PhasePending, got.Status.Phase)
}

func TestOrch_ChecksARestoreAgainstItsSnapshotsVolumes(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	_, _, err := o.garam.CreateMachine(ctx, "src", machineSpec())
	require.NoError(t, err)
	_, _, err = o.garam.PutSnapshot(ctx, "snap", machineapi.SnapshotRequest{Machine: "src"})
	require.NoError(t, err)
	from := machineSpec()
	from.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "snap"}
	_, created, err := o.garam.CreateMachine(ctx, "restored", from)
	require.NoError(t, err, "volumes that match")
	assert.True(t, created)

	larger := from
	larger.Volumes = []machineapi.Volume{{Name: "data", SizeBytes: 2 << 30, MountPath: "/data"}}
	_, _, err = o.garam.CreateMachine(ctx, "larger", larger)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, "a volume of another size")
	absent := machineSpec()
	absent.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "absent"}
	_, _, err = o.garam.CreateMachine(ctx, "absent", absent)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotNotFound, "a snapshot the caller has not")
}

func TestOrch_KeepsSnapshotsIdempotentByName(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	for _, n := range []string{"a", "b"} {
		_, _, err := o.garam.CreateMachine(ctx, n, machineSpec())
		require.NoError(t, err)
	}
	s, created, err := o.garam.PutSnapshot(ctx, "s", machineapi.SnapshotRequest{Machine: "a"})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, machineapi.SnapshotPending, s.Phase, "the reconciler takes it on the host")
	assert.Equal(t, machineapi.Redacted, s.Spec.Env["TOKEN"], "with no secret answered")
	_, created, err = o.garam.PutSnapshot(ctx, "s", machineapi.SnapshotRequest{Machine: "a"})
	require.NoError(t, err)
	assert.False(t, created, "the same Machine answers 200")
	_, _, err = o.garam.PutSnapshot(ctx, "s", machineapi.SnapshotRequest{Machine: "b"})
	refused(t, err, http.StatusConflict, machineapi.CodeConflict, "another Machine under the name")
	list, err := o.garam.Snapshots(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, o.garam.DeleteSnapshot(ctx, "s"))
	_, err = o.garam.Snapshot(ctx, "s")
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "a deleted snapshot")
}

func TestOrch_SetsTheDesiredStateAndHoldsADeletedName(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	m, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	stopped, err := o.garam.StopMachine(ctx, "web", machineapi.StopRequest{GraceSeconds: 30})
	require.NoError(t, err)
	assert.Equal(t, machineapi.StateStopped, stopped.Spec.State)
	assert.Equal(t, m.Status.Generation+1, stopped.Status.Generation, "a change of the state is a new generation")
	again, err := o.garam.StopMachine(ctx, "web", machineapi.StopRequest{})
	require.NoError(t, err)
	assert.Equal(t, stopped.Status.Generation, again.Status.Generation, "a repeat changes nothing")
	started, err := o.garam.StartMachine(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, stopped.Status.Generation+1, started.Status.Generation)

	require.NoError(t, o.garam.DeleteMachine(ctx, "web"))
	got, err := o.garam.Machine(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, machineapi.PhaseDeleting, got.Status.Phase, "the reconciler removes it once its volumes are gone")
	require.NoError(t, o.garam.DeleteMachine(ctx, "web"), "a repeated delete is accepted")
	_, _, err = o.garam.CreateMachine(ctx, "web", machineSpec())
	refused(t, err, http.StatusConflict, machineapi.CodeDeleting, "a create while the name is held")
	_, err = o.garam.StartMachine(ctx, "web")
	refused(t, err, http.StatusConflict, machineapi.CodeDeleting, "a start while it is deleted")
}

func TestOrch_ServesEachCallersEventsByCursor(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	list, err := o.garam.Machines(ctx)
	require.NoError(t, err)
	m, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	_, _, err = o.functions.CreateMachine(ctx, "theirs", machineSpec())
	require.NoError(t, err)

	first, err := o.garam.Events(ctx, list.Cursor)
	require.NoError(t, err)
	require.Len(t, first.Events, 1, "the create, and nothing of another caller's")
	assert.Equal(t, machineapi.Event{Type: machineapi.EventStatus, Machine: "web", ID: m.ID, Status: m.Status, Time: first.Events[0].Time}, first.Events[0])

	_, err = o.garam.StopMachine(ctx, "web", machineapi.StopRequest{})
	require.NoError(t, err)
	require.NoError(t, o.garam.DeleteMachine(ctx, "web"))
	second, err := o.garam.Events(ctx, first.Cursor)
	require.NoError(t, err)
	require.Len(t, second.Events, 2, "the stop and the delete, and not the create again")
	assert.Equal(t, m.Status.Generation+1, second.Events[0].Status.Generation)
	assert.Equal(t, machineapi.PhaseDeleting, second.Events[1].Status.Phase)
	third, err := o.garam.Events(ctx, second.Cursor)
	require.NoError(t, err)
	assert.Empty(t, third.Events)

	for _, bad := range []string{"not-a-cursor", "-1", "999999999"} {
		_, err = o.garam.Events(ctx, bad)
		refused(t, err, http.StatusBadRequest, machineapi.CodeInvalid, "the cursor "+bad)
	}
}

func TestOrch_RefusesARevokedCaller(t *testing.T) {
	o := startMachineOrch(t)
	_, err := o.garam.Machines(t.Context())
	require.NoError(t, err, "control: the caller before its revocation")
	o.c.CA.Revoke(o.garamID)
	_, err = o.garam.Machines(t.Context())
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "a revoked caller")
	var e *machineapi.Error
	require.True(t, errors.As(err, &e))
	assert.Contains(t, e.Message, "revoked", "refused for its revocation, not for its role")
	_, err = o.functions.Machines(t.Context())
	require.NoError(t, err, "control: another caller is served")
}

func TestOrch_AnswersAGuestCallToAMachineNotPlacedAsNotRunning(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	_, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	_, err = o.garam.Exec(ctx, "web", machineapi.ExecRequest{Command: []string{"cat"}})
	refused(t, err, http.StatusConflict, machineapi.CodeNotRunning, "exec")
	_, err = o.garam.ReadFile(ctx, "web", "/data/a")
	refused(t, err, http.StatusConflict, machineapi.CodeNotRunning, "a file read")
}

// get answers a GET of path on addr to a client presenting id, as status and body.
func get(t *testing.T, c *orchtest.Cluster, addr, path string, id orchtest.Identity) (int, string) {
	t.Helper()
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: c.CA.Pool(), Certificates: []tls.Certificate{id.TLS},
	}}}
	resp, err := hc.Get("https://" + addr + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func TestOrch_ServesTheMachineAPIOnItsOwnListenerAlone(t *testing.T) {
	o := startMachineOrch(t)
	m := o.c.Members[0]
	reader := o.c.CA.Issue(t, "rita", identity.RoleReader)
	status, _ := get(t, o.c, m.Addr(), "/v1/hosts", reader)
	require.Equal(t, http.StatusOK, status, "control: the operator API serves a reader on its own listener")

	status, body := get(t, o.c, m.MachineAPIAddr(), "/v1/hosts", o.garamID)
	assert.Equal(t, http.StatusNotFound, status, "the Machine API's listener serves no operator route")
	assert.Contains(t, body, `"code":"not-found"`)
	status, _ = get(t, o.c, m.MachineAPIAddr(), "/v1/hosts", reader)
	assert.Equal(t, http.StatusForbidden, status, "nor any role but caller")
	status, _ = get(t, o.c, m.Addr(), machineapi.PathPrefix+"machines", o.garamID)
	assert.Equal(t, http.StatusNotFound, status, "and the operator API serves no Machine API route")
	status, _ = get(t, o.c, m.MachineAPIAddr(), machineapi.PathPrefix+"machines", o.garamID)
	assert.Equal(t, http.StatusOK, status, "control: the Machine API serves its caller")
}

func TestOrch_ShowsOperatorsEveryCallersMachines(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	m, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	reader := o.c.CA.Issue(t, "rita", identity.RoleReader)
	all, err := o.c.Client(t, 0, &reader).Machines(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "garam", all[0].Caller)
	assert.Equal(t, m.ID, all[0].ID)
	assert.Nil(t, all[0].Binding, "bound to no host")
	assert.Equal(t, machineapi.Redacted, all[0].Spec.Env["TOKEN"], "with no secret shown")

	status, body := get(t, o.c, o.c.Members[0].Addr(), "/v1/machines", o.garamID)
	assert.Equal(t, http.StatusForbidden, status, "a caller reads nothing of the operator API: %s", strings.TrimSpace(body))
}

func TestOrch_AnswersTheMachineAPIUnavailableWhileItWaitsForARestore(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Restore: true, MachineZone: machineZone})
	cl := c.MachineClient(t, 0, c.CA.Issue(t, "garam", identity.RoleCaller))
	_, err := cl.Machines(t.Context())
	refused(t, err, http.StatusServiceUnavailable, machineapi.CodeUnavailable, "a member without its data")
}

func TestOrch_ServesNoMachineAPIWithoutAZone(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	assert.Empty(t, c.Members[0].MachineAPIAddr(), "no zone, no listener")
	z := orchtest.Start(t, orchtest.Options{Members: 1, MachineZone: machineZone})
	assert.NotEmpty(t, z.Members[0].MachineAPIAddr(), "control: a zone serves it")
}

// A caller's keys end in a "/" after its name, so a caller whose name begins another's reads none
// of the other's.
func TestOrch_KeepsACallerApartFromOneWhoseNameExtendsIts(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	garam2 := o.c.MachineClient(t, 0, o.c.CA.Issue(t, "garam2", identity.RoleCaller))
	_, _, err := garam2.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	_, _, err = garam2.PutSnapshot(ctx, "before", machineapi.SnapshotRequest{Machine: "web"})
	require.NoError(t, err)

	for who, cl := range map[string]*machineapi.Client{"garam2, control": garam2, "garam": o.garam} {
		own := who != "garam"
		list, err := cl.Machines(ctx)
		require.NoError(t, err)
		assert.Equal(t, own, len(list.Machines) == 1, "%s: Machines %v", who, list.Machines)
		snaps, err := cl.Snapshots(ctx)
		require.NoError(t, err)
		assert.Equal(t, own, len(snaps) == 1, "%s: snapshots %v", who, snaps)
		page, err := cl.Events(ctx, "")
		require.NoError(t, err)
		assert.Equal(t, own, len(page.Events) == 1, "%s: events %v", who, page.Events)
	}
}

// Every member is started with the same --machine-zone; a member started with another serves 503,
// so no Machine is named under a second zone.
func TestOrch_RefusesTheMachineAPIOnAMemberOfAnotherZone(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	_, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	require.NoError(t, o.c.Members[0].Stop())

	o.c.RestartInZone(t, 0, "other.example.test")
	_, err = o.c.MachineClient(t, 0, o.garamID).Machines(ctx)
	refused(t, err, http.StatusServiceUnavailable, machineapi.CodeUnavailable, "a member of another zone")
	var e *machineapi.Error
	require.ErrorAs(t, err, &e)
	assert.Contains(t, e.Message, machineZone, "it names the cluster's zone")

	require.NoError(t, o.c.Members[0].Stop())
	o.c.RestartInZone(t, 0, machineZone)
	m, err := o.c.MachineClient(t, 0, o.garamID).Machine(ctx, "web")
	require.NoError(t, err, "control: the cluster's zone")
	assert.True(t, strings.HasSuffix(m.Status.Address.Name, "."+machineZone), m.Status.Address.Name)
}
