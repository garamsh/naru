// Package machineapifake is an in-memory implementation of the Machine API, machines/v1alpha1: the
// second implementation the version has while it is below v1 (ADR 0108 item 2). It passes
// machineapitest, and a caller's tests can run against it in place of orch.
//
// It runs no guest. A Machine reaches the phase its desired state asks for Latency after each
// write, as a reconciler's pass would make it; its guest's files are a map, and its exec runs
// Options.Exec.
package machineapifake

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/machineapi"
)

// Options configure a Fake.
type Options struct {
	// Zone is the zone stable names and aliases are under, such as "m.garam.sh".
	Zone string
	// AliasDomains are the domains outside the zone each caller may publish aliases in, as an
	// operator grants them.
	AliasDomains map[string][]string
	// Latency is how long a Machine takes to reach the phase a write asks for, a snapshot to be
	// ready and a deleted Machine to be gone. Zero is 200 ms.
	Latency time.Duration
	// LogSize is how many events each caller's log keeps. Zero is 20000.
	LogSize int
	// LogAge is how old an event the log keeps. Zero is 7 days.
	LogAge time.Duration
	// Exec runs a command in a guest. Nil runs "cat", which answers stdin on stdout, and answers
	// exit code 127 to any other command.
	Exec func(command []string, stdin []byte) machineapi.ExecResult
}

// Fake is the in-memory Machine API. It is an http.Handler, served over mutual TLS: the caller is
// the CommonName of a client certificate whose one OrganizationalUnit is "caller".
type Fake struct {
	o   Options
	mux *http.ServeMux

	mu      sync.Mutex
	callers map[string]*account
	// aliases are every caller's, by hostname: an alias is unique across callers.
	aliases map[string]*alias
}

type account struct {
	machines  map[string]*machine
	snapshots map[string]*snapshot
	log       []logged
	// seq is the sequence number of the newest event; a cursor is a sequence number.
	seq uint64
}

type logged struct {
	seq   uint64
	event machineapi.Event
}

type machine struct {
	m     machineapi.Machine
	spec  machineapi.Spec // unredacted, as created
	files map[string][]byte
}

// snapshot is a snapshot and the guest files it took: those under its Machine's volumes.
type snapshot struct {
	s     machineapi.Snapshot
	files map[string][]byte
}

type alias struct {
	caller string
	req    machineapi.AliasRequest
	read   machineapi.Alias
}

// New returns a Fake that holds nothing.
func New(o Options) *Fake {
	if o.Latency == 0 {
		o.Latency = 200 * time.Millisecond
	}
	if o.LogSize == 0 {
		o.LogSize = 20000
	}
	if o.LogAge == 0 {
		o.LogAge = 7 * 24 * time.Hour
	}
	if o.Exec == nil {
		o.Exec = cat
	}
	f := &Fake{o: o, callers: map[string]*account{}, aliases: map[string]*alias{}}
	f.routes()
	return f
}

func cat(command []string, stdin []byte) machineapi.ExecResult {
	if slices.Equal(command, []string{"cat"}) {
		return machineapi.ExecResult{Stdout: stdin, Stderr: []byte{}}
	}
	return machineapi.ExecResult{ExitCode: 127, Stdout: []byte{}, Stderr: fmt.Appendf(nil, "%s: not found\n", command)}
}

// refusal is an answer of the API that is not a success.
type refusal struct {
	status int
	code   machineapi.Code
	msg    string
}

func (r *refusal) Error() string { return r.msg }

func refuse(status int, code machineapi.Code, format string, a ...any) error {
	return &refusal{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

func (f *Fake) account(caller string) *account {
	a := f.callers[caller]
	if a == nil {
		a = &account{machines: map[string]*machine{}, snapshots: map[string]*snapshot{}}
		f.callers[caller] = a
	}
	return a
}

// emit appends the Machine's status to its caller's log and trims the log to what it keeps.
func (f *Fake) emit(a *account, typ machineapi.EventType, m machineapi.Machine) {
	now := time.Now().UTC()
	a.seq++
	a.log = append(a.log, logged{seq: a.seq, event: machineapi.Event{Type: typ, Machine: m.Name, ID: m.ID, Status: m.Status, Time: now}})
	drop := max(len(a.log)-f.o.LogSize, 0)
	for drop < len(a.log) && now.Sub(a.log[drop].event.Time) > f.o.LogAge {
		drop++
	}
	a.log = a.log[drop:]
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Fake) address(id string) machineapi.Address {
	return machineapi.Address{Name: id + "." + f.o.Zone}
}

// capabilities are what v1alpha1's implementation offers every Machine (ADR 0108 item 8).
func capabilities(s machineapi.Spec) machineapi.Capabilities {
	c := machineapi.Capabilities{Snapshot: []machineapi.SnapshotKind{machineapi.SnapshotVolumes}, Move: []machineapi.MoveKind{machineapi.MoveCold}}
	if s.Shape.GPU != nil {
		g := *s.Shape.GPU
		c.GPU = &g
	} else {
		c.Resize = []machineapi.ResizeKind{machineapi.ResizeCPU, machineapi.ResizeMemory}
	}
	return c
}

// settleAfter has the Machine id reach the phase its desired state asks for after the latency.
func (f *Fake) settleAfter(caller, name, id string) {
	time.AfterFunc(f.o.Latency, func() { f.settle(caller, name, id) })
}

func (f *Fake) settle(caller, name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	mc := a.machines[name]
	if mc == nil || mc.m.ID != id {
		return
	}
	st := &mc.m.Status
	if st.Phase == machineapi.PhaseDeleting {
		delete(a.machines, name)
		f.emit(a, machineapi.EventDeleted, mc.m)
		return
	}
	phase := machineapi.PhaseRunning
	if mc.m.Spec.State == machineapi.StateStopped {
		phase = machineapi.PhaseStopped
	}
	if st.Phase == phase && st.ObservedGeneration == st.Generation {
		return
	}
	st.Phase, st.ObservedGeneration = phase, st.Generation
	f.emit(a, machineapi.EventStatus, mc.m)
}

func (f *Fake) create(caller, name string, spec machineapi.Spec) (machineapi.Machine, bool, error) {
	if err := machineapi.CheckName(name); err != nil {
		return machineapi.Machine{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "name: %v", err)
	}
	if err := spec.Check(); err != nil {
		return machineapi.Machine{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "%v", err)
	}
	if spec.State == "" {
		spec.State = machineapi.StateRunning
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	if mc := a.machines[name]; mc != nil {
		switch {
		case mc.m.Status.Phase == machineapi.PhaseDeleting:
			return machineapi.Machine{}, false, refuse(http.StatusConflict, machineapi.CodeDeleting, "the Machine %s is being deleted, and its name is held until its volumes are gone", name)
		case !machineapi.SameSpec(mc.spec, spec):
			return machineapi.Machine{}, false, refuse(http.StatusConflict, machineapi.CodeConflict, "the Machine %s exists with another spec", name)
		}
		return mc.m, false, nil
	}
	if src := spec.FromSnapshot; src != nil {
		snap := a.snapshots[src.Snapshot]
		if snap == nil {
			return machineapi.Machine{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeSnapshotNotFound, "no snapshot %s", src.Snapshot)
		}
		if err := machineapi.MatchVolumes(snap.s.Spec.Volumes, spec.Volumes); err != nil {
			return machineapi.Machine{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, "%v", err)
		}
	}
	id := newID()
	mc := &machine{spec: spec, files: map[string][]byte{}}
	if src := spec.FromSnapshot; src != nil {
		for p, b := range a.snapshots[src.Snapshot].files {
			mc.files[p] = b
		}
	}
	for _, fl := range spec.Files {
		mc.files[fl.Path] = fl.Content
	}
	mc.m = machineapi.Machine{Name: name, ID: id, Spec: spec.Redact(), Status: machineapi.Status{
		Phase: machineapi.PhasePending, Address: f.address(id), Generation: 1, Capabilities: capabilities(spec),
	}}
	a.machines[name] = mc
	f.emit(a, machineapi.EventStatus, mc.m)
	f.settleAfter(caller, name, id)
	return mc.m, true, nil
}

// owned returns the caller's Machine name; another caller's is not found.
func (f *Fake) owned(caller, name string) (*account, *machine, error) {
	a := f.account(caller)
	mc := a.machines[name]
	if mc == nil {
		return a, nil, refuse(http.StatusNotFound, machineapi.CodeNotFound, "no Machine %s", name)
	}
	return a, mc, nil
}

func (f *Fake) machine(caller, name string) (machineapi.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, mc, err := f.owned(caller, name)
	if err != nil {
		return machineapi.Machine{}, err
	}
	return mc.m, nil
}

func (f *Fake) machines(caller string) machineapi.MachineList {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	l := machineapi.MachineList{Machines: []machineapi.Machine{}, Cursor: strconv.FormatUint(a.seq, 10)}
	for _, n := range slices.Sorted(maps.Keys(a.machines)) {
		l.Machines = append(l.Machines, a.machines[n].m)
	}
	return l
}

// setState sets the desired state; a repeat changes nothing.
func (f *Fake) setState(caller, name string, want machineapi.State) (machineapi.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, mc, err := f.owned(caller, name)
	if err != nil {
		return machineapi.Machine{}, err
	}
	if mc.m.Status.Phase == machineapi.PhaseDeleting {
		return machineapi.Machine{}, refuse(http.StatusConflict, machineapi.CodeDeleting, "the Machine %s is being deleted", name)
	}
	if mc.m.Spec.State == want {
		return mc.m, nil
	}
	mc.spec.State, mc.m.Spec.State = want, want
	st := &mc.m.Status
	st.Generation++
	st.Phase = machineapi.PhaseStarting
	if want == machineapi.StateStopped {
		st.Phase = machineapi.PhaseStopping
	}
	f.emit(a, machineapi.EventStatus, mc.m)
	f.settleAfter(caller, name, mc.m.ID)
	return mc.m, nil
}

func (f *Fake) deleteMachine(caller, name string) (machineapi.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, mc, err := f.owned(caller, name)
	if err != nil {
		return machineapi.Machine{}, err
	}
	if mc.m.Status.Phase == machineapi.PhaseDeleting {
		return mc.m, nil
	}
	for h, al := range f.aliases {
		if al.caller == caller && al.req.Machine == name {
			delete(f.aliases, h)
		}
	}
	mc.m.Status.Phase = machineapi.PhaseDeleting
	f.emit(a, machineapi.EventStatus, mc.m)
	f.settleAfter(caller, name, mc.m.ID)
	return mc.m, nil
}

// running returns the caller's Machine name when it runs, for an exec or a file call.
func (f *Fake) running(caller, name string) (*machine, error) {
	_, mc, err := f.owned(caller, name)
	if err != nil {
		return nil, err
	}
	if mc.m.Status.Phase != machineapi.PhaseRunning {
		return nil, refuse(http.StatusConflict, machineapi.CodeNotRunning, "the Machine %s is %s, not running", name, mc.m.Status.Phase)
	}
	return mc, nil
}

func (f *Fake) exec(caller, name string, r machineapi.ExecRequest) (machineapi.ExecResult, error) {
	if len(r.Command) == 0 {
		return machineapi.ExecResult{}, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "command: is empty")
	}
	f.mu.Lock()
	_, err := f.running(caller, name)
	f.mu.Unlock()
	if err != nil {
		return machineapi.ExecResult{}, err
	}
	return f.o.Exec(r.Command, r.Stdin), nil
}

func (f *Fake) readFile(caller, name, p string) ([]byte, error) {
	if err := machineapi.CheckPath(p); err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "path: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mc, err := f.running(caller, name)
	if err != nil {
		return nil, err
	}
	b, ok := mc.files[p]
	if !ok {
		return nil, refuse(http.StatusNotFound, machineapi.CodeFileNotFound, "no file %s in the Machine %s", p, name)
	}
	return slices.Clone(b), nil
}

func (f *Fake) writeFile(caller, name, p string, b []byte) error {
	if err := machineapi.CheckPath(p); err != nil {
		return refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "path: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mc, err := f.running(caller, name)
	if err != nil {
		return err
	}
	mc.files[p] = b
	return nil
}

// ExpireLog drops every event of the caller's log, as age would: a cursor the log gave before is
// then older than it keeps. It is a test's stand-in for time passing.
func (f *Fake) ExpireLog(caller string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	a.log = nil
}

func (f *Fake) events(caller, since string) (machineapi.EventPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	after := uint64(0)
	if since != "" {
		n, err := strconv.ParseUint(since, 10, 64)
		if err != nil || n > a.seq {
			return machineapi.EventPage{}, refuse(http.StatusBadRequest, machineapi.CodeInvalid, "since: %q is not a cursor of this log", since)
		}
		after = n
	}
	oldest := a.seq + 1
	if len(a.log) > 0 {
		oldest = a.log[0].seq
	}
	if since != "" && after+1 < oldest {
		return machineapi.EventPage{}, refuse(http.StatusGone, machineapi.CodeCursorExpired, "the cursor %d is older than the log keeps: list the Machines again", after)
	}
	p := machineapi.EventPage{Events: []machineapi.Event{}, Cursor: strconv.FormatUint(a.seq, 10)}
	for _, l := range a.log {
		if l.seq > after {
			p.Events = append(p.Events, l.event)
		}
	}
	return p, nil
}

func (f *Fake) putSnapshot(caller, name string, r machineapi.SnapshotRequest) (machineapi.Snapshot, bool, error) {
	if err := machineapi.CheckName(name); err != nil {
		return machineapi.Snapshot{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "name: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a, mc, err := f.owned(caller, r.Machine)
	if err != nil {
		return machineapi.Snapshot{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "no Machine %s", r.Machine)
	}
	if s := a.snapshots[name]; s != nil {
		if s.s.MachineID != mc.m.ID {
			return machineapi.Snapshot{}, false, refuse(http.StatusConflict, machineapi.CodeConflict, "the snapshot %s is of another Machine", name)
		}
		return s.s, false, nil
	}
	if mc.m.Status.Phase == machineapi.PhaseDeleting {
		return machineapi.Snapshot{}, false, refuse(http.StatusConflict, machineapi.CodeDeleting, "the Machine %s is being deleted", r.Machine)
	}
	s := &snapshot{s: machineapi.Snapshot{Name: name, Machine: r.Machine, MachineID: mc.m.ID, Spec: mc.m.Spec,
		Phase: machineapi.SnapshotPending, CreatedAt: time.Now().UTC()}, files: map[string][]byte{}}
	for p, b := range mc.files {
		if slices.ContainsFunc(mc.spec.Volumes, func(v machineapi.Volume) bool { return strings.HasPrefix(p, v.MountPath+"/") }) {
			s.files[p] = slices.Clone(b)
		}
	}
	a.snapshots[name] = s
	time.AfterFunc(f.o.Latency, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if a.snapshots[name] == s {
			s.s.Phase = machineapi.SnapshotReady
		}
	})
	return s.s, true, nil
}

func (f *Fake) snapshot(caller, name string) (machineapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.account(caller).snapshots[name]
	if s == nil {
		return machineapi.Snapshot{}, refuse(http.StatusNotFound, machineapi.CodeNotFound, "no snapshot %s", name)
	}
	return s.s, nil
}

func (f *Fake) snapshots(caller string) machineapi.SnapshotList {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	l := machineapi.SnapshotList{Snapshots: []machineapi.Snapshot{}}
	for _, n := range slices.Sorted(maps.Keys(a.snapshots)) {
		l.Snapshots = append(l.Snapshots, a.snapshots[n].s)
	}
	return l
}

func (f *Fake) deleteSnapshot(caller, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.account(caller)
	if a.snapshots[name] == nil {
		return refuse(http.StatusNotFound, machineapi.CodeNotFound, "no snapshot %s", name)
	}
	delete(a.snapshots, name)
	return nil
}

func (f *Fake) putAlias(caller, hostname string, r machineapi.AliasRequest) (machineapi.Alias, bool, error) {
	inZone, err := machineapi.AliasInZone(hostname, f.o.Zone)
	if err != nil {
		return machineapi.Alias{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "%v", err)
	}
	read := machineapi.Alias{Hostname: hostname, Machine: r.Machine, Service: r.Service}
	switch {
	case inZone && (r.Certificate != "" || r.Key != ""):
		return machineapi.Alias{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "an alias in the zone %s takes no certificate or key", f.o.Zone)
	case !inZone && !slices.ContainsFunc(f.o.AliasDomains[caller], func(d string) bool { return machineapi.InDomain(hostname, d) }):
		return machineapi.Alias{}, false, refuse(http.StatusForbidden, machineapi.CodeAliasDomainNotGranted, "%s is outside the zone and in no domain granted to the caller", hostname)
	case !inZone:
		cert, err := machineapi.CheckAliasCertificate(hostname, r.Certificate, r.Key, time.Now())
		if err != nil {
			return machineapi.Alias{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "%v", err)
		}
		read.Certificate, read.Key = &cert, machineapi.Redacted
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.aliases[hostname]
	if held != nil && held.caller != caller {
		return machineapi.Alias{}, false, refuse(http.StatusConflict, machineapi.CodeAliasTaken, "another caller holds %s", hostname)
	}
	_, mc, err := f.owned(caller, r.Machine)
	if err != nil || mc.m.Status.Phase == machineapi.PhaseDeleting {
		return machineapi.Alias{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "no Machine %s", r.Machine)
	}
	svc := mc.spec.Services
	if read.Service == "" && len(svc) > 0 {
		read.Service = svc[0].Name
	}
	if !slices.ContainsFunc(svc, func(s machineapi.Service) bool { return s.Name == read.Service }) {
		return machineapi.Alias{}, false, refuse(http.StatusUnprocessableEntity, machineapi.CodeInvalid, "service: the Machine %s has no service %q", r.Machine, read.Service)
	}
	if held != nil {
		if held.req != r {
			return machineapi.Alias{}, false, refuse(http.StatusConflict, machineapi.CodeConflict, "the alias %s exists for another Machine, service or certificate", hostname)
		}
		return held.read, false, nil
	}
	f.aliases[hostname] = &alias{caller: caller, req: r, read: read}
	return read, true, nil
}

func (f *Fake) alias(caller, hostname string) (machineapi.Alias, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	al := f.aliases[hostname]
	if al == nil || al.caller != caller {
		return machineapi.Alias{}, refuse(http.StatusNotFound, machineapi.CodeNotFound, "no alias %s", hostname)
	}
	return al.read, nil
}

func (f *Fake) deleteAlias(caller, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	al := f.aliases[hostname]
	if al == nil || al.caller != caller {
		return refuse(http.StatusNotFound, machineapi.CodeNotFound, "no alias %s", hostname)
	}
	delete(f.aliases, hostname)
	return nil
}

// errorOf returns the status, code and message an error answers.
func errorOf(err error) (int, machineapi.ErrorBody) {
	var r *refusal
	if errors.As(err, &r) {
		return r.status, machineapi.ErrorBody{Error: r.msg, Code: r.code}
	}
	return http.StatusInternalServerError, machineapi.ErrorBody{Error: "internal error", Code: machineapi.CodeInternal}
}
