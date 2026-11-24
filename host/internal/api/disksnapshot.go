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

// diskSnapshotView is a disk snapshot as the host API shows it: the microVM and disk it was taken of
// are read back from the disk's identifier, <microVM>.<disk>.
func diskSnapshotView(d mvmd.DiskSnapshot) hostapi.DiskSnapshot {
	microVM, disk, _ := strings.Cut(d.DiskID, ".")
	return hostapi.DiskSnapshot{
		Name: d.ID, MicroVM: microVM, Disk: disk, SizeBytes: d.SizeBytes, AllocatedBytes: d.AllocatedBytes, CreatedAt: d.CreatedAt,
	}
}

// snapshotDisk copies a disk of a microVM into a disk snapshot, which outlives the microVM. A running
// microVM is paused for the copy and runs again, and the copy is crash-consistent. The same name
// for the same disk again is the snapshot already taken; the name of a snapshot of another disk is
// 409. A microVM, or a disk of it, the host does not hold is 404.
func (s *Server) snapshotDisk(w http.ResponseWriter, r *http.Request) {
	var req hostapi.DiskSnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	if !dnsLabel.MatchString(req.Name) {
		invalid(w, "name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	name, disk := r.PathValue("name"), r.PathValue("disk")
	m, err := s.deps.Runtime.InspectMachine(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := diskID(name, disk)
	if !slices.ContainsFunc(m.Disks, func(a mvmd.Attachment) bool { return a.ID == id }) {
		s.fail(w, fmt.Errorf("the microVM %s has no disk %s: %w", name, disk, mvmd.ErrNotFound))
		return
	}
	d, err := s.deps.Runtime.SnapshotDisk(ctx, req.Name, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, diskSnapshotView(d))
}

func (s *Server) listDiskSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	all, err := s.deps.Runtime.ListDiskSnapshots(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]hostapi.DiskSnapshot, 0, len(all))
	for _, d := range all {
		out = append(out, diskSnapshotView(d))
	}
	slices.SortFunc(out, func(a, b hostapi.DiskSnapshot) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{"diskSnapshots": out})
}

// deleteDiskSnapshot deletes a disk snapshot; an absent one is 204, so a retry succeeds. A disk made
// from it is a copy and stays.
func (s *Server) deleteDiskSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Runtime.DeleteDiskSnapshot(ctx, r.PathValue("name")); err != nil && !errors.Is(err, mvmd.ErrNotFound) {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
