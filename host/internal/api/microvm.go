package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// callTimeout bounds a create, which pulls an image and boots the Machine (mvmd's own bounds are
// shorter than this), and a delete.
const callTimeout = 5 * time.Minute

// maxBody bounds a request body.
const maxBody = 1 << 20

// MicroVM is a microVM as the host API shows it.
type MicroVM struct {
	Name string `json:"name"`
	// MachineID is mvmd's identifier of the Machine, which is the name.
	MachineID     string      `json:"machineId"`
	State         string      `json:"state"`
	Failure       string      `json:"failure,omitempty"`
	Addresses     []string    `json:"addresses"`
	ImageDigest   string      `json:"imageDigest"`
	CPU           uint32      `json:"cpu"`
	Memory        uint64      `json:"memory"`
	RootDiskBytes uint64      `json:"rootDiskBytes"`
	Disks         []DiskSpec  `json:"disks"`
	NetworkPolicy mvmd.Policy `json:"networkPolicy"`
}

func view(m mvmd.Machine, sizes map[string]uint64) MicroVM {
	v := MicroVM{
		Name: m.ID, MachineID: m.ID, State: m.State, Failure: m.Failure, Addresses: m.Addresses,
		ImageDigest: m.ImageDigest, CPU: m.CPU, Memory: m.MemoryBytes, RootDiskBytes: m.RootDiskBytes,
		Disks: []DiskSpec{}, NetworkPolicy: m.Policy,
	}
	if v.Addresses == nil {
		v.Addresses = []string{}
	}
	for _, d := range m.Disks {
		v.Disks = append(v.Disks, DiskSpec{Name: diskName(m.ID, d.ID), SizeBytes: sizes[d.ID], ReadOnly: d.ReadOnly})
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
	writeJSON(w, status, errorBody{Error: err.Error()})
}

func (s *Server) createMicroVM(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "the body is not one JSON object of the fields the API names"})
		return
	}
	p, fields, msg := validate(req)
	if len(fields) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: msg, Fields: fields})
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
			writeJSON(w, http.StatusConflict, errorBody{Error: "a microVM named " + req.Name + " exists with another body"})
			return
		}
		// The first attempt may have ended after the create and before the start.
		if existing.State == mvmd.StateCreated || existing.State == mvmd.StateStopped {
			if existing, err = rt.StartMachine(ctx, req.Name); err != nil {
				s.fail(w, err)
				return
			}
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
	writeJSON(w, http.StatusCreated, view(m, sizes))
}

// provision pulls the image, makes the disks and the Machine, and starts it. A failure before the
// Machine exists removes the disks this request's own CreateDisk calls made, and no other: a disk
// whose CreateDisk was refused belongs to another request.
func (s *Server) provision(ctx context.Context, p plan) (mvmd.Machine, map[string]uint64, error) {
	rt := s.deps.Runtime
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
			s.dropDisks(made)
			return mvmd.Machine{}, nil, err
		}
		made = append(made, id)
		sizes[id] = d.SizeBytes
	}
	m, err := rt.CreateMachine(ctx, p.create)
	if err != nil {
		s.dropDisks(made)
		return mvmd.Machine{}, nil, err
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

// dropDisks deletes the disks, by ID, that a request made for a Machine it did not create. It uses
// a context of its own, since the request's may be what ended.
func (s *Server) dropDisks(ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range ids {
		if err := s.deps.Runtime.DeleteDisk(ctx, id); err != nil && !errors.Is(err, mvmd.ErrNotFound) {
			s.deps.Log.Error("cannot remove a disk of a microVM that was not created", "disk", id, "err", err.Error())
		}
	}
}

// matches reports whether the Machine mvmd holds is the one the plan would create: the same
// image, resources, root disk, network policy and disks, each disk of the same size.
func matches(m mvmd.Machine, sizes map[string]uint64, p plan) bool {
	c := p.create
	if m.ImageDigest != c.ImageDigest || m.CPU != c.CPU || m.MemoryBytes != c.MemoryBytes || m.RootDiskBytes != c.RootDiskBytes {
		return false
	}
	if !samePolicy(m.Policy, c.Policy) {
		return false
	}
	byID := func(a, b mvmd.Attachment) int { return strings.Compare(a.ID, b.ID) }
	have, want := slices.SortedFunc(slices.Values(m.Disks), byID), slices.SortedFunc(slices.Values(c.Disks), byID)
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
	out := make([]MicroVM, 0, len(machines))
	for _, m := range machines {
		out = append(out, view(m, sizes))
	}
	slices.SortFunc(out, func(a, b MicroVM) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{"microvms": out})
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
