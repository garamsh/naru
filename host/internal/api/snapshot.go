package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

func snapshotView(s mvmd.Snapshot) hostapi.Snapshot {
	v := hostapi.Snapshot{
		Name: s.ID, MicroVM: s.MachineID, ImageDigest: s.ImageDigest, Driver: s.Driver, CPUModel: s.CPUModel,
		Kernel: s.Kernel, RootDiskBytes: s.RootDisk, Disks: []hostapi.SnapshotDisk{}, SizeBytes: s.SizeBytes, CreatedAt: s.CreatedAt,
	}
	for _, d := range s.Disks {
		v.Disks = append(v.Disks, hostapi.SnapshotDisk{ID: d.ID, SizeBytes: d.SizeBytes, ReadOnly: d.ReadOnly})
	}
	return v
}

// findSnapshot returns the snapshot mvmd holds under name, or mvmd.ErrNotFound: mvmd has no call
// that inspects one.
func (s *Server) findSnapshot(ctx context.Context, name string) (mvmd.Snapshot, error) {
	all, err := s.deps.Runtime.ListSnapshots(ctx)
	if err != nil {
		return mvmd.Snapshot{}, err
	}
	for _, sn := range all {
		if sn.ID == name {
			return sn, nil
		}
	}
	return mvmd.Snapshot{}, fmt.Errorf("snapshot %s: %w", name, mvmd.ErrNotFound)
}

// snapshotMicroVM takes a snapshot of a running or paused microVM, which stays as it is. The same
// name for the same microVM again is the snapshot already taken. mvmd refuses the snapshot of an
// OS-form microVM and of one whose driver cannot, and that is 409 with its reason.
func (s *Server) snapshotMicroVM(w http.ResponseWriter, r *http.Request) {
	var req hostapi.SnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	if !dnsLabel.MatchString(req.Name) {
		invalid(w, "name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	if err := s.deps.Runtime.SnapshotMachine(ctx, req.Name, r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	sn, err := s.findSnapshot(ctx, req.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshotView(sn))
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	all, err := s.deps.Runtime.ListSnapshots(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]hostapi.Snapshot, 0, len(all))
	for _, sn := range all {
		out = append(out, snapshotView(sn))
	}
	slices.SortFunc(out, func(a, b hostapi.Snapshot) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sn, err := s.findSnapshot(ctx, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotView(sn))
}

// snapshotDisks returns the disks the snapshots hold references to, which are the disks they need
// to resume, except those of the snapshot named except.
func (s *Server) snapshotDisks(ctx context.Context, except string) (map[string]bool, error) {
	snaps, err := s.deps.Runtime.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, sn := range snaps {
		if sn.ID == except {
			continue
		}
		for _, d := range sn.Disks {
			out[d.ID] = true
		}
	}
	return out, nil
}

// deleteSnapshot deletes a snapshot and the disks it was holding for a resume that no other
// snapshot and no microVM needs; an absent one is 204. One a microVM is being resumed from is 409.
func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rt := s.deps.Runtime
	name := r.PathValue("name")
	sn, err := s.findSnapshot(ctx, name)
	if err != nil && !errors.Is(err, mvmd.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if err := rt.DeleteSnapshot(ctx, name); err != nil {
		s.fail(w, err)
		return
	}
	if len(sn.Disks) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	needed, err := s.snapshotDisks(ctx, name)
	if err == nil {
		var machines []mvmd.Machine
		if machines, err = rt.ListMachines(ctx); err == nil {
			for _, m := range machines {
				for _, d := range m.Disks {
					needed[d.ID] = true
				}
			}
		}
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, d := range sn.Disks {
		if needed[d.ID] {
			continue
		}
		if err := rt.DeleteDisk(ctx, d.ID); err != nil && !errors.Is(err, mvmd.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// resumeSnapshot makes a new running microVM from a snapshot: mvmd restores a snapshot into a
// Machine of its own and does not resume the one it was taken of. The microVM has the snapshot's
// disks, which mvmd requires to be held by no Machine and unchanged, and the policy and routed
// prefix of the request, which a snapshot does not carry.
func (s *Server) resumeSnapshot(w http.ResponseWriter, r *http.Request) {
	var req hostapi.ResumeRequest
	if !decode(w, r, &req) {
		return
	}
	var fields []string
	if !dnsLabel.MatchString(req.Name) {
		fields = append(fields, "name")
	}
	var routed []string
	if req.PodPrefix != "" {
		if validRoutedPrefix(req.PodPrefix) {
			routed = []string{req.PodPrefix}
		} else {
			fields = append(fields, "podPrefix")
		}
	}
	if req.Network != "" && !dnsLabel.MatchString(req.Network) {
		fields = append(fields, "network")
	}
	fields = append(fields, checkRules("networkPolicy.egress", req.NetworkPolicy.Egress)...)
	fields = append(fields, checkRules("networkPolicy.ingress", req.NetworkPolicy.Ingress)...)
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	if req.Network != "" && !s.checkNetwork(w, req.Network, req.PodPrefix) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	m, err := s.deps.Runtime.RestoreMachine(ctx, mvmd.Restore{
		ID: req.Name, SnapshotID: r.PathValue("name"), Policy: req.NetworkPolicy, RoutedPrefixes: routed,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.attach(ctx, req.Network, req.PodPrefix, m); err != nil {
		// The microVM runs; delete it and keep its disks, which are the snapshot's.
		_ = s.deps.Runtime.StopMachine(ctx, m.ID)
		_ = s.deps.Runtime.DeleteMachine(ctx, m.ID)
		s.networkFail(w, err)
		return
	}
	sizes, err := diskSizes(ctx, s.deps.Runtime)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(m, sizes))
}
