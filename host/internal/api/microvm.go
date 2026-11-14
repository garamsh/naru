package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// callTimeout bounds a create, which pulls an image and boots the Machine (mvmd's own bounds are
// shorter than this), and a delete.
const callTimeout = 5 * time.Minute

// maxBody bounds a request body. An OS-form body carries the seed's user-data in base64, and the
// seed volume may be as large as mvmd takes (1 MiB), which is more than a quarter larger here.
const maxBody = 4 << 20

func view(m mvmd.Machine, sizes map[string]uint64) hostapi.MicroVM {
	v := hostapi.MicroVM{
		Name: m.ID, MachineID: m.ID, State: m.State, Failure: m.Failure, Addresses: m.Addresses,
		ImageDigest: m.ImageDigest, CPU: m.CPU, Memory: m.MemoryBytes, RootDiskBytes: m.RootDiskBytes,
		Disks: []hostapi.DiskSpec{}, NetworkPolicy: m.Policy, BootSet: m.BootSet, Cmdline: m.Cmdline,
		RestartOnBoot: m.RestartOnBoot, Fenced: m.Fenced, FenceLapsedAt: m.FenceLapsedAt,
	}
	if len(m.RoutedPrefixes) > 0 {
		v.PodPrefix = m.RoutedPrefixes[0]
	}
	if v.Addresses == nil {
		v.Addresses = []string{}
	}
	for _, d := range m.Disks {
		v.Disks = append(v.Disks, hostapi.DiskSpec{Name: diskName(m.ID, d.ID), SizeBytes: sizes[d.ID], ReadOnly: d.ReadOnly})
	}
	return v
}

func diskSizes(ctx context.Context, rt mvmd.Runtime) (map[string]uint64, error) {
	disks, err := rt.ListDisks(ctx)
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]uint64, len(disks))
	for _, d := range disks {
		sizes[d.ID] = d.SizeBytes
	}
	return sizes, nil
}

// fail answers the error of a call to mvmd with the status that names its cause.
func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, mvmd.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, mvmd.ErrConflict), errors.Is(err, mvmd.ErrPrecondition):
		status = http.StatusConflict
	case errors.Is(err, mvmd.ErrInvalid):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, mvmd.ErrUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	}
	if status == http.StatusBadGateway {
		s.deps.Log.Error("mvmd failed a call", "err", err.Error())
	}
	writeJSON(w, status, hostapi.ErrorBody{Error: err.Error()})
}

func (s *Server) createMicroVM(w http.ResponseWriter, r *http.Request) {
	var req hostapi.CreateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body is not one JSON object of the fields the API names"})
		return
	}
	p, fields, msg := validate(req)
	if len(fields) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: msg, Fields: fields})
		return
	}
	if p.create.BootSet != "" && !s.deps.MachineGateway.IsValid() {
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started with no --machine-gateway, so it cannot write the network-config of an OS-form microVM"})
		return
	}
	if req.Network != "" && !s.checkNetwork(w, req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	rt := s.deps.Runtime

	existing, err := rt.InspectMachine(ctx, req.Name)
	switch {
	case err == nil:
		sizes, err := diskSizes(ctx, rt)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !matches(existing, sizes, p) {
			writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "a microVM named " + req.Name + " exists with another body"})
			return
		}
		// The first attempt may have ended after the create and before the start. A fenced microVM
		// is started by its caller, once it has renewed the fence.
		if !existing.Fenced && (existing.State == mvmd.StateCreated || existing.State == mvmd.StateStopped) {
			if existing, err = rt.StartMachine(ctx, req.Name); err != nil {
				s.fail(w, err)
				return
			}
		}
		// A repeat attaches again, which a host that restarted since needs.
		if err := s.attach(ctx, req, existing); err != nil {
			s.networkFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view(existing, sizes))
		return
	case !errors.Is(err, mvmd.ErrNotFound):
		s.fail(w, err)
		return
	}

	m, sizes, err := s.provision(ctx, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.attach(ctx, req, m); err != nil {
		ids := make([]string, 0, len(sizes))
		for id := range sizes {
			ids = append(ids, id)
		}
		// The microVM runs unless it is fenced, and mvmd deletes only a Machine that does not.
		if m.State == mvmd.StateRunning || !p.create.Fenced {
			_ = rt.StopMachine(ctx, m.ID)
		}
		s.dropMade(m.ID, ids)
		s.networkFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view(m, sizes))
}

// provision makes what the plan describes and starts the Machine, unless it is fenced. The image form pulls the image,
// makes the disks, then the Machine. The OS form makes the data disk, then the Machine, then the
// seed, whose network-config needs the address mvmd gave the Machine, and attaches the seed
// read-only before the start. A failure before the start removes what this request made, and no
// more: a disk or Machine whose creation was refused belongs to another request.
func (s *Server) provision(ctx context.Context, p plan) (mvmd.Machine, map[string]uint64, error) {
	rt := s.deps.Runtime
	osForm := p.create.BootSet != ""
	if p.repository != "" {
		if err := rt.Pull(ctx, p.repository, p.create.ImageDigest); err != nil {
			return mvmd.Machine{}, nil, err
		}
	}
	sizes := map[string]uint64{}
	var made []string
	for _, d := range p.disks {
		id := diskID(p.create.ID, d.Name)
		if err := rt.CreateDisk(ctx, id, d.SizeBytes); err != nil {
			s.dropMade("", made)
			return mvmd.Machine{}, nil, err
		}
		made = append(made, id)
		sizes[id] = d.SizeBytes
	}
	m, err := rt.CreateMachine(ctx, p.create)
	if err != nil {
		s.dropMade("", made)
		return mvmd.Machine{}, nil, err
	}
	if osForm {
		seedID, seedSize, err := s.makeSeed(ctx, p, m, &made)
		if err != nil {
			s.dropMade(m.ID, made)
			return mvmd.Machine{}, nil, err
		}
		sizes[seedID] = seedSize
	}
	if p.create.Fenced {
		return m, sizes, nil
	}
	started, err := rt.StartMachine(ctx, m.ID)
	if errors.Is(err, mvmd.ErrPrecondition) {
		// A concurrent identical request started it.
		started, err = rt.InspectMachine(ctx, m.ID)
	}
	if err != nil {
		return mvmd.Machine{}, nil, err
	}
	return started, sizes, nil
}

// makeSeed creates the seed disk of an OS-form Machine from the address mvmd gave it, and attaches
// it read-only. made gains the disk once its creation succeeded.
func (s *Server) makeSeed(ctx context.Context, p plan, m mvmd.Machine, made *[]string) (string, uint64, error) {
	rt := s.deps.Runtime
	if len(m.Addresses) == 0 {
		return "", 0, errors.New("mvmd gave the Machine no address")
	}
	addr, err := netip.ParseAddr(m.Addresses[0])
	if err != nil {
		return "", 0, err
	}
	info, err := rt.Info(ctx)
	if err != nil {
		return "", 0, err
	}
	var prefix netip.Prefix
	for _, c := range info.MachinePrefixes {
		if pf, err := netip.ParsePrefix(c); err == nil && pf.Contains(addr) {
			prefix = netip.PrefixFrom(addr, pf.Bits())
		}
	}
	if !prefix.IsValid() {
		return "", 0, errors.New("the Machine's address is in none of the prefixes mvmd reports")
	}
	vol, err := seedVolume(m.ID, p.userData, prefix, s.deps.MachineGateway)
	if err != nil {
		return "", 0, err
	}
	id := diskID(m.ID, seedDiskName)
	if err := rt.CreateRawDisk(ctx, id, 0, vol); err != nil {
		return "", 0, err
	}
	*made = append(*made, id)
	if _, err := rt.AttachDisk(ctx, m.ID, mvmd.Attachment{ID: id, ReadOnly: true}); err != nil {
		return "", 0, err
	}
	return id, uint64(len(vol)), nil
}

// dropMade deletes what a request made that did not end in a started Machine: the Machine, when
// this request made it, and then the disks by ID. It uses a context of its own, since the
// request's may be what ended.
func (s *Server) dropMade(machine string, disks []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if machine != "" {
		if err := s.deps.Runtime.DeleteMachine(ctx, machine); err != nil {
			s.deps.Log.Error("cannot remove a microVM that was not started", "machine", machine, "err", err.Error())
		}
	}
	for _, id := range disks {
		if err := s.deps.Runtime.DeleteDisk(ctx, id); err != nil && !errors.Is(err, mvmd.ErrNotFound) {
			s.deps.Log.Error("cannot remove a disk of a microVM that was not created", "disk", id, "err", err.Error())
		}
	}
}

// matches reports whether the Machine mvmd holds is the one the plan would create: the same
// image or boot set, resources, root disk, network policy, routed prefixes, restart on boot, fence and disks, each disk of
// the same size. An OS-form Machine also has its seed attached read-only; the seed's contents are
// not compared, since they hold the address mvmd chose.
func matches(m mvmd.Machine, sizes map[string]uint64, p plan) bool {
	c := p.create
	if m.ImageDigest != c.ImageDigest || m.CPU != c.CPU || m.MemoryBytes != c.MemoryBytes || m.RootDiskBytes != c.RootDiskBytes ||
		m.RestartOnBoot != c.RestartOnBoot || m.Fenced != c.Fenced {
		return false
	}
	if !samePolicy(m.Policy, c.Policy) || m.BootSet != c.BootSet || m.Cmdline != c.Cmdline ||
		!slices.Equal(m.RoutedPrefixes, c.RoutedPrefixes) {
		return false
	}
	wantDisks := slices.Clone(c.Disks)
	if c.BootSet != "" {
		wantDisks = append(wantDisks, mvmd.Attachment{ID: diskID(c.ID, seedDiskName), ReadOnly: true})
	}
	byID := func(a, b mvmd.Attachment) int { return strings.Compare(a.ID, b.ID) }
	have, want := slices.SortedFunc(slices.Values(m.Disks), byID), slices.SortedFunc(slices.Values(wantDisks), byID)
	if !slices.Equal(have, want) {
		return false
	}
	for _, d := range p.disks {
		if sizes[diskID(c.ID, d.Name)] != d.SizeBytes {
			return false
		}
	}
	return true
}

// samePolicy compares two policies, an absent list being an empty one.
func samePolicy(a, b mvmd.Policy) bool {
	norm := func(r []mvmd.Rule) []mvmd.Rule {
		out := make([]mvmd.Rule, 0, len(r))
		for _, x := range r {
			if len(x.Ports) == 0 {
				x.Ports = nil
			}
			out = append(out, x)
		}
		return out
	}
	return reflect.DeepEqual(norm(a.Egress), norm(b.Egress)) && reflect.DeepEqual(norm(a.Ingress), norm(b.Ingress))
}

func (s *Server) getMicroVM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	m, err := s.deps.Runtime.InspectMachine(ctx, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	sizes, err := diskSizes(ctx, s.deps.Runtime)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(m, sizes))
}

func (s *Server) listMicroVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	machines, err := s.deps.Runtime.ListMachines(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	sizes, err := diskSizes(ctx, s.deps.Runtime)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]hostapi.MicroVM, 0, len(machines))
	for _, m := range machines {
		out = append(out, view(m, sizes))
	}
	slices.SortFunc(out, func(a, b hostapi.MicroVM) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{"microvms": out})
}

// stopMicroVM stops the Machine if it runs. A Machine that is not running is answered as it is, so
// a repeat is 200; mvmd restarts none that was stopped, after a host's boot or otherwise.
func (s *Server) stopMicroVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	rt := s.deps.Runtime
	m, err := rt.InspectMachine(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if m.State == mvmd.StateRunning || m.State == mvmd.StatePaused {
		if err := rt.StopMachine(ctx, name); err != nil && !errors.Is(err, mvmd.ErrPrecondition) {
			s.fail(w, err)
			return
		}
		if m, err = rt.InspectMachine(ctx, name); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.answerMicroVM(ctx, w, m)
}

// startMicroVM starts the Machine unless it runs, which is answered as it is, so a repeat is 200.
// A Machine mvmd refuses to start in its state is 409 with mvmd's reason.
func (s *Server) startMicroVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	rt := s.deps.Runtime
	m, err := rt.InspectMachine(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if m.State != mvmd.StateRunning {
		if m, err = rt.StartMachine(ctx, name); errors.Is(err, mvmd.ErrPrecondition) {
			// A concurrent start may have won.
			var now mvmd.Machine
			if now, err = rt.InspectMachine(ctx, name); err == nil && now.State != mvmd.StateRunning {
				err = mvmd.ErrPrecondition
			}
			m = now
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	s.answerMicroVM(ctx, w, m)
}

// answerMicroVM writes 200 with the microVM as it is.
func (s *Server) answerMicroVM(ctx context.Context, w http.ResponseWriter, m mvmd.Machine) {
	sizes, err := diskSizes(ctx, s.deps.Runtime)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(m, sizes))
}

// deleteMicroVM stops the Machine if it runs, deletes it, and deletes the disks that were made
// with it. An absent name is 404, whether it never existed or this is the retry of a delete that
// succeeded; a retry also removes the disks a delete that stopped half way left.
func (s *Server) deleteMicroVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	rt := s.deps.Runtime
	m, err := rt.InspectMachine(ctx, name)
	if errors.Is(err, mvmd.ErrNotFound) {
		s.removeOrphans(ctx, name)
		s.fail(w, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if m.State == mvmd.StateRunning || m.State == mvmd.StatePaused {
		if err := rt.StopMachine(ctx, name); err != nil && !errors.Is(err, mvmd.ErrPrecondition) {
			s.fail(w, err)
			return
		}
	}
	if err := rt.DeleteMachine(ctx, name); err != nil {
		s.fail(w, err)
		return
	}
	s.detach(ctx, m)
	for _, d := range m.Disks {
		if !strings.HasPrefix(d.ID, name+".") {
			continue
		}
		if err := rt.DeleteDisk(ctx, d.ID); err != nil && !errors.Is(err, mvmd.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeOrphans deletes the unattached disks that carry the name's prefix.
func (s *Server) removeOrphans(ctx context.Context, name string) {
	disks, err := s.deps.Runtime.ListDisks(ctx)
	if err != nil {
		return
	}
	for _, d := range disks {
		if strings.HasPrefix(d.ID, name+".") && d.Holder == "" {
			if err := s.deps.Runtime.DeleteDisk(ctx, d.ID); err != nil {
				s.deps.Log.Error("cannot remove a disk left by a delete", "disk", d.ID, "err", err.Error())
			}
		}
	}
}
