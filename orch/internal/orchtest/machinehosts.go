package orchtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// FakeMachineHosts stands in for the hosts' APIs as the Machine reconciler reaches them. Every orch
// member shares one, as the members share the real hosts. It answers as hostd does: a create of a
// name it holds is the microVM it holds, a missing microVM or disk snapshot is 404, and a stop or
// delete of one that is gone is no error to the reconciler. Its guests hold files: those the create
// placed and those written since, a file under a disk's mount path on that disk, which a disk
// snapshot copies and a disk made from it holds; an exec runs "cat" over them.
type FakeMachineHosts struct {
	mu    sync.Mutex
	hosts map[string]*fakeMachineHost
	// DeleteDelay holds each delete of a microVM that long, so a delete outlasts a round trip of
	// the Machine API as a real host's does.
	DeleteDelay time.Duration
	// AfterList, when not nil, runs once each List has taken its answer, before it returns it.
	AfterList func()
	// made counts the microVMs made, each given an address of 10.200.0.0/16 as mvmd gives one,
	// unless NoAddresses, as before a microVM's host reports one.
	made        int
	NoAddresses bool
}

type fakeMachineHost struct {
	host  machines.Host
	down  bool
	stale bool
	vms   map[string]hostapi.MicroVM
	calls int
	disks map[string]hostapi.DiskSnapshot
	// creates are every create asked, and graces every stop's grace.
	creates []hostapi.CreateRequest
	graces  []uint32
	// refuse answers each create, and refuseDisks each disk snapshot, with this error when not nil.
	refuse, refuseDisks error
	// hang, when not nil, holds every call but List until it closes.
	hang chan struct{}
	// roots are each microVM's files on no disk, volumes each disk's by its path below its mount
	// (keyed <microVM>/<disk>), and snapFiles each disk snapshot's; execs are every exec asked.
	roots     map[string]map[string][]byte
	volumes   map[string]map[string][]byte
	snapFiles map[string]map[string][]byte
	execs     []hostapi.ExecRequest
	// grants are the host's exports not yet fetched, imported the disks imports made that no
	// microVM holds yet (keyed <microVM>.<disk>), and refuseImports answers every import when not
	// nil.
	grants        map[string]fakeGrant
	imported      map[string]map[string][]byte
	refuseImports error
	// exports and imports count the calls asked.
	exports, imports int
}

// fakeGrant is one export: the disk's files, for the host by ID.
type fakeGrant struct {
	target string
	files  map[string][]byte
	sha    string
}

var _ machines.Hosts = (*FakeMachineHosts)(nil)

// NewFakeMachineHosts returns the hosts, each answering with nothing on it.
func NewFakeMachineHosts(hosts ...machines.Host) *FakeMachineHosts {
	f := &FakeMachineHosts{hosts: map[string]*fakeMachineHost{}}
	for _, h := range hosts {
		f.hosts[h.ID] = &fakeMachineHost{
			host: h, vms: map[string]hostapi.MicroVM{}, disks: map[string]hostapi.DiskSnapshot{},
			roots: map[string]map[string][]byte{}, volumes: map[string]map[string][]byte{}, snapFiles: map[string]map[string][]byte{},
			grants: map[string]fakeGrant{}, imported: map[string]map[string][]byte{},
		}
	}
	return f
}

// Fail makes the host stop answering, and Recover answer again; what it holds stays.
func (f *FakeMachineHosts) Fail(id string)    { f.set(id, func(h *fakeMachineHost) { h.down = true }) }
func (f *FakeMachineHosts) Recover(id string) { f.set(id, func(h *fakeMachineHost) { h.down = false }) }

// Abandon marks the host abandoned, as its inventory record does once an operator abandons it. It
// answers as before, so a test sees that the record alone, not the host's silence, is acted on.
func (f *FakeMachineHosts) Abandon(id string) {
	f.set(id, func(h *fakeMachineHost) { h.host.Abandoned = true })
}

// Readmit clears the host's abandonment, as a host removed and admitted again under its ID is
// recorded.
func (f *FakeMachineHosts) Readmit(id string) {
	f.set(id, func(h *fakeMachineHost) { h.host.Abandoned = false })
}

// Reset makes the host as one removed, reset and admitted again under its ID: not abandoned, and
// holding no microVM.
func (f *FakeMachineHosts) Reset(id string) {
	f.set(id, func(h *fakeMachineHost) { h.host.Abandoned, h.vms = false, map[string]hostapi.MicroVM{} })
}

// Remove takes the host out of the list, as orch's removal of it does.
func (f *FakeMachineHosts) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hosts, id)
}

// Drain sets whether the host is draining, as the inventory records it.
func (f *FakeMachineHosts) Drain(id string, draining bool) {
	f.set(id, func(h *fakeMachineHost) { h.host.Draining = draining })
}

// Stale makes List give the host's answer as one its probe repeated, as a host that missed one
// probe is given.
func (f *FakeMachineHosts) Stale(id string, stale bool) {
	f.set(id, func(h *fakeMachineHost) { h.stale = stale })
}

// Refuse makes the host answer every create with err, and RefuseDisks every disk snapshot; nil
// stops it.
func (f *FakeMachineHosts) Refuse(id string, err error) {
	f.set(id, func(h *fakeMachineHost) { h.refuse = err })
}
func (f *FakeMachineHosts) RefuseDisks(id string, err error) {
	f.set(id, func(h *fakeMachineHost) { h.refuseDisks = err })
}

// Hang holds every call to the host but List until the function it returns is called.
func (f *FakeMachineHosts) Hang(id string) (release func()) {
	ch := make(chan struct{})
	f.set(id, func(h *fakeMachineHost) { h.hang = ch })
	var once sync.Once
	return func() {
		once.Do(func() {
			f.set(id, func(h *fakeMachineHost) { h.hang = nil })
			close(ch)
		})
	}
}

// PutDiskSnapshot places a disk snapshot on the host behind the reconciler's back.
func (f *FakeMachineHosts) PutDiskSnapshot(id string, d hostapi.DiskSnapshot) {
	f.set(id, func(h *fakeMachineHost) { h.disks[d.Name] = d })
}

// Graces returns every stop's grace the host was given, in order.
func (f *FakeMachineHosts) Graces(id string) []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts[id].graces)
}

// Exit stops a microVM as if it stopped on its own, in state (stopped or faulted).
func (f *FakeMachineHosts) Exit(id, microVM, state string) {
	f.set(id, func(h *fakeMachineHost) {
		vm := h.vms[microVM]
		vm.State = state
		h.vms[microVM] = vm
	})
}

// Put places a microVM on the host behind the reconciler's back.
func (f *FakeMachineHosts) Put(id string, vm hostapi.MicroVM) {
	f.set(id, func(h *fakeMachineHost) { h.vms[vm.Name] = vm })
}

func (f *FakeMachineHosts) set(id string, change func(h *fakeMachineHost)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.hosts[id])
}

// MicroVMs returns the microVMs on the host by name.
func (f *FakeMachineHosts) MicroVMs(id string) map[string]hostapi.MicroVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.hosts[id].vms)
}

// DiskSnapshots returns the names of the host's disk snapshots, in order.
func (f *FakeMachineHosts) DiskSnapshots(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.hosts[id].disks))
}

// Creates returns every create the host was asked, in order.
func (f *FakeMachineHosts) Creates(id string) []hostapi.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts[id].creates)
}

func (f *FakeMachineHosts) List(context.Context) ([]machines.Host, error) {
	out := f.list()
	if f.AfterList != nil {
		f.AfterList()
	}
	return out, nil
}

func (f *FakeMachineHosts) list() []machines.Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []machines.Host
	for _, id := range slices.Sorted(maps.Keys(f.hosts)) {
		h := f.hosts[id]
		out = append(out, h.host)
		if h.down {
			continue
		}
		o := &out[len(out)-1]
		o.Answering, o.Stale = true, h.stale
		for _, n := range slices.Sorted(maps.Keys(h.vms)) {
			o.MicroVMs = append(o.MicroVMs, h.vms[n])
		}
		for _, n := range slices.Sorted(maps.Keys(h.disks)) {
			o.DiskSnapshots = append(o.DiskSnapshots, h.disks[n])
		}
	}
	return out
}

// answering returns the host when it answers, after any hang; the caller holds f.mu, which this
// releases while it hangs.
func (f *FakeMachineHosts) answering(id string) (*fakeMachineHost, error) {
	h, ok := f.hosts[id]
	if ok && h.hang != nil {
		ch := h.hang
		f.mu.Unlock()
		<-ch
		f.mu.Lock()
	}
	if !ok || h.down {
		return nil, fmt.Errorf("host %s does not answer", id)
	}
	h.calls++
	return h, nil
}

// Calls counts the calls the host answered, every call but List.
func (f *FakeMachineHosts) Calls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.hosts[id]; h != nil {
		return h.calls
	}
	return 0
}

func notFound(format string, a ...any) error {
	return &hostapi.Error{Status: http.StatusNotFound, Message: fmt.Sprintf(format, a...)}
}

func (f *FakeMachineHosts) Create(_ context.Context, host string, r hostapi.CreateRequest) (hostapi.MicroVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.MicroVM{}, err
	}
	h.creates = append(h.creates, r)
	if h.refuse != nil {
		return hostapi.MicroVM{}, h.refuse
	}
	if vm, ok := h.vms[r.Name]; ok {
		return vm, nil
	}
	for _, d := range r.Disks {
		if d.FromSnapshot != "" && h.disks[d.FromSnapshot].Name == "" {
			return hostapi.MicroVM{}, notFound("no disk snapshot %s", d.FromSnapshot)
		}
		if _, ok := h.imported[r.Name+"."+d.Name]; d.Imported && !ok {
			return hostapi.MicroVM{}, &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "no imported disk " + r.Name + "." + d.Name}
		}
	}
	f.made++
	vm := hostapi.MicroVM{
		Name: r.Name, MachineID: r.Name, State: "running", ImageDigest: r.Image, CPU: r.CPU, Memory: r.Memory,
		RootDiskBytes: r.RootDiskBytes, Disks: r.Disks, NetworkPolicy: r.NetworkPolicy,
	}
	if !f.NoAddresses {
		vm.Addresses = []string{fmt.Sprintf("10.200.%d.%d", f.made/250, 1+f.made%250)}
	}
	if r.NoStart {
		vm.State = "created"
	}
	h.vms[r.Name] = vm
	h.roots[r.Name] = map[string][]byte{}
	for _, d := range r.Disks {
		h.volumes[r.Name+"/"+d.Name] = maps.Clone(h.snapFiles[d.FromSnapshot])
		if d.Imported {
			h.volumes[r.Name+"/"+d.Name] = h.imported[r.Name+"."+d.Name]
			delete(h.imported, r.Name+"."+d.Name)
		}
		if h.volumes[r.Name+"/"+d.Name] == nil {
			h.volumes[r.Name+"/"+d.Name] = map[string][]byte{}
		}
	}
	for _, file := range r.Files {
		files, key := h.place(vm, file.Path)
		files[key] = slices.Clone(file.Content)
	}
	return vm, nil
}

func (f *FakeMachineHosts) state(host, microVM, state string) (hostapi.MicroVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.MicroVM{}, err
	}
	vm, ok := h.vms[microVM]
	if !ok {
		return hostapi.MicroVM{}, notFound("no microVM %s", microVM)
	}
	vm.State = state
	h.vms[microVM] = vm
	return vm, nil
}

func (f *FakeMachineHosts) Start(_ context.Context, host, microVM string) (hostapi.MicroVM, error) {
	return f.state(host, microVM, "running")
}

func (f *FakeMachineHosts) Stop(_ context.Context, host, microVM string, grace uint32) (hostapi.MicroVM, error) {
	f.mu.Lock()
	if h, ok := f.hosts[host]; ok {
		h.graces = append(h.graces, grace)
	}
	f.mu.Unlock()
	vm, err := f.state(host, microVM, "stopped")
	if he, ok := err.(*hostapi.Error); ok && he.Status == http.StatusNotFound {
		return hostapi.MicroVM{Name: microVM, State: "stopped"}, nil
	}
	return vm, err
}

func (f *FakeMachineHosts) Delete(ctx context.Context, host, microVM string) error {
	f.mu.Lock()
	_, err := f.answering(host)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.DeleteDelay):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[host]
	if vm, ok := h.vms[microVM]; ok {
		for _, d := range vm.Disks {
			delete(h.volumes, microVM+"/"+d.Name)
		}
	}
	delete(h.vms, microVM)
	delete(h.roots, microVM)
	maps.DeleteFunc(h.imported, func(k string, _ map[string][]byte) bool { return strings.HasPrefix(k, microVM+".") })
	return nil
}

func (f *FakeMachineHosts) SnapshotDisk(_ context.Context, host, microVM, disk, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return err
	}
	if h.refuseDisks != nil {
		return h.refuseDisks
	}
	vm, ok := h.vms[microVM]
	if !ok {
		return notFound("no microVM %s", microVM)
	}
	i := slices.IndexFunc(vm.Disks, func(d hostapi.DiskSpec) bool { return d.Name == disk })
	if i < 0 {
		return notFound("the microVM %s has no disk %s", microVM, disk)
	}
	if _, ok := h.disks[name]; !ok {
		size := vm.Disks[i].SizeBytes
		h.disks[name] = hostapi.DiskSnapshot{Name: name, MicroVM: microVM, Disk: disk, SizeBytes: size, AllocatedBytes: size}
		h.snapFiles[name] = maps.Clone(h.volumes[microVM+"/"+disk])
	}
	return nil
}

func (f *FakeMachineHosts) DeleteDiskSnapshot(_ context.Context, host, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return err
	}
	delete(h.disks, name)
	delete(h.snapFiles, name)
	return nil
}

// place is where the microVM's guest keeps the file at p: under its path below the mount path of
// the disk that holds it, or under p on no disk.
func (h *fakeMachineHost) place(vm hostapi.MicroVM, p string) (files map[string][]byte, key string) {
	for _, d := range vm.Disks {
		if d.MountPath != "" && strings.HasPrefix(p, d.MountPath+"/") {
			return h.volumes[vm.Name+"/"+d.Name], strings.TrimPrefix(p, d.MountPath)
		}
	}
	return h.roots[vm.Name], p
}

// guest returns the host's microVM when it runs, for a guest call.
func (f *FakeMachineHosts) guest(host, microVM string) (*fakeMachineHost, hostapi.MicroVM, error) {
	h, err := f.answering(host)
	if err != nil {
		return nil, hostapi.MicroVM{}, fmt.Errorf("%w: %v", machines.ErrHostUnavailable, err)
	}
	vm, ok := h.vms[microVM]
	if !ok || vm.State != "running" {
		return nil, hostapi.MicroVM{}, fmt.Errorf("%w: %s", machines.ErrNotRunning, microVM)
	}
	return h, vm, nil
}

// Exec refuses what a host refuses, after it records the call; it runs "cat" over the guest's
// files, answers "sleep" as a command its timeout ended, and 127 to any other command.
func (f *FakeMachineHosts) Exec(_ context.Context, host, microVM string, r hostapi.ExecRequest) (hostapi.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, vm, err := f.guest(host, microVM)
	if err != nil {
		return hostapi.ExecResult{}, err
	}
	h.execs = append(h.execs, r)
	if _, err := r.Check(); err != nil {
		return hostapi.ExecResult{}, fmt.Errorf("%w: %v", machineapi.ErrInvalid, err)
	}
	if r.Command[0] == "sleep" {
		return hostapi.ExecResult{ExitCode: 137, Signal: 9, TimedOut: true, Stdout: []byte{}, Stderr: []byte{}}, nil
	}
	if r.Command[0] != "cat" {
		return hostapi.ExecResult{ExitCode: 127, Stdout: []byte{}, Stderr: []byte(r.Command[0] + ": not found")}, nil
	}
	out := hostapi.ExecResult{Stdout: []byte{}, Stderr: []byte{}}
	for _, p := range r.Command[1:] {
		files, key := h.place(vm, p)
		b, ok := files[key]
		if !ok {
			out.ExitCode, out.Stderr = 1, fmt.Appendf(out.Stderr, "cat: %s: No such file or directory\n", p)
			continue
		}
		out.Stdout = append(out.Stdout, b...)
	}
	return out, nil
}

func (f *FakeMachineHosts) ReadFile(_ context.Context, host, microVM, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, vm, err := f.guest(host, microVM)
	if err != nil {
		return nil, err
	}
	files, key := h.place(vm, path)
	b, ok := files[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", machines.ErrFileNotFound, path)
	}
	return slices.Clone(b), nil
}

func (f *FakeMachineHosts) WriteFile(_ context.Context, host, microVM, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, vm, err := f.guest(host, microVM)
	if err != nil {
		return err
	}
	files, key := h.place(vm, path)
	files[key] = slices.Clone(content)
	return nil
}

// Execs returns every exec the host was asked, in order.
func (f *FakeMachineHosts) Execs(id string) []hostapi.ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts[id].execs)
}

// RefuseImports makes the host answer every import with err; nil stops it.
func (f *FakeMachineHosts) RefuseImports(id string, err error) {
	f.set(id, func(h *fakeMachineHost) { h.refuseImports = err })
}

// Transfers returns how many exports and imports the host was asked.
func (f *FakeMachineHosts) Transfers(id string) (exports, imports int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[id].exports, f.hosts[id].imports
}

// filesDigest is the SHA-256 the fake gives a disk: over its files' paths and contents in order.
func filesDigest(files map[string][]byte) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%d:%s%d:", len(k), k, len(files[k]))
		h.Write(files[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ExportDisk grants the target one fetch of the disk <microVM>.<disk>, which a running microVM's
// disk is not, as hostd refuses a disk a running Machine holds.
func (f *FakeMachineHosts) ExportDisk(_ context.Context, host, disk, target string) (hostapi.Export, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.Export{}, err
	}
	h.exports++
	microVM, name, _ := strings.Cut(disk, ".")
	vm, ok := h.vms[microVM]
	i := slices.IndexFunc(vm.Disks, func(d hostapi.DiskSpec) bool { return d.Name == name })
	if !ok || i < 0 {
		return hostapi.Export{}, notFound("no disk %s", disk)
	}
	if vm.State == "running" || vm.State == "paused" {
		return hostapi.Export{}, &hostapi.Error{Status: http.StatusConflict, Message: "the disk " + disk + " is held by a " + vm.State + " microVM"}
	}
	files := maps.Clone(h.volumes[microVM+"/"+name])
	g := fakeGrant{target: target, files: files, sha: filesDigest(files)}
	grant := fmt.Sprintf("%064x", len(h.grants)+h.exports)
	h.grants[grant] = g
	return hostapi.Export{Grant: grant, SizeBytes: vm.Disks[i].SizeBytes, Filesystem: hostapi.FilesystemExt4, SHA256: g.sha, ExpiresAt: time.Now().Add(hostapi.GrantSeconds * time.Second)}, nil
}

// ImportDisk fetches the source's grant once, as only its target may, and makes the disk; the same
// disk of the same digest again is success.
func (f *FakeMachineHosts) ImportDisk(_ context.Context, host string, r hostapi.ImportRequest) (hostapi.Imported, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.Imported{}, err
	}
	h.imports++
	if h.refuseImports != nil {
		return hostapi.Imported{}, h.refuseImports
	}
	out := hostapi.Imported{Name: r.Name, SizeBytes: r.SizeBytes, Filesystem: r.Filesystem, SHA256: r.SHA256}
	if have, ok := h.imported[r.Name]; ok && filesDigest(have) == r.SHA256 {
		return out, nil
	}
	src, err := f.answering(r.SourceHost)
	if err != nil {
		return hostapi.Imported{}, &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeSourceUnavailable, Message: err.Error()}
	}
	g, ok := src.grants[r.Grant]
	delete(src.grants, r.Grant)
	if !ok || g.target != host {
		return hostapi.Imported{}, &hostapi.Error{Status: http.StatusConflict, Code: hostapi.CodeGrantRefused, Message: "the source refused the grant"}
	}
	if g.sha != r.SHA256 {
		return hostapi.Imported{}, &hostapi.Error{Status: http.StatusConflict, Code: hostapi.CodeDigestMismatch, Message: "the stream's digest is not the request's"}
	}
	h.imported[r.Name] = maps.Clone(g.files)
	return out, nil
}
