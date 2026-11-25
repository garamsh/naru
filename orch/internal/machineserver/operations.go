package machineserver

import (
	"net/http"

	"github.com/garamsh/naru/lib/machineapi"
)

func createdOr(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

func (h *handler) create(r *http.Request, caller string) (int, any, error) {
	var spec machineapi.Spec
	if err := decode(r, &spec); err != nil {
		return 0, nil, err
	}
	m, created, err := h.Machines.Create(r.Context(), caller, r.PathValue("name"), spec)
	return createdOr(created), m, err
}

func (h *handler) list(r *http.Request, caller string) (int, any, error) {
	l, err := h.Machines.Machines(r.Context(), caller)
	return http.StatusOK, l, err
}

func (h *handler) get(r *http.Request, caller string) (int, any, error) {
	m, err := h.Machines.Machine(r.Context(), caller, r.PathValue("name"))
	return http.StatusOK, m, err
}

func (h *handler) start(r *http.Request, caller string) (int, any, error) {
	m, err := h.Machines.Start(r.Context(), caller, r.PathValue("name"))
	return http.StatusOK, m, err
}

func (h *handler) stop(r *http.Request, caller string) (int, any, error) {
	var s machineapi.StopRequest
	if err := decode(r, &s); err != nil {
		return 0, nil, err
	}
	m, err := h.Machines.Stop(r.Context(), caller, r.PathValue("name"), s)
	return http.StatusOK, m, err
}

func (h *handler) delete(r *http.Request, caller string) (int, any, error) {
	m, err := h.Machines.Delete(r.Context(), caller, r.PathValue("name"))
	return http.StatusAccepted, m, err
}

func (h *handler) putSnapshot(r *http.Request, caller string) (int, any, error) {
	var s machineapi.SnapshotRequest
	if err := decode(r, &s); err != nil {
		return 0, nil, err
	}
	snap, created, err := h.Machines.PutSnapshot(r.Context(), caller, r.PathValue("name"), s)
	return createdOr(created), snap, err
}

func (h *handler) snapshots(r *http.Request, caller string) (int, any, error) {
	l, err := h.Machines.Snapshots(r.Context(), caller)
	return http.StatusOK, machineapi.SnapshotList{Snapshots: l}, err
}

func (h *handler) snapshot(r *http.Request, caller string) (int, any, error) {
	s, err := h.Machines.Snapshot(r.Context(), caller, r.PathValue("name"))
	return http.StatusOK, s, err
}

func (h *handler) deleteSnapshot(r *http.Request, caller string) (int, any, error) {
	return http.StatusNoContent, nil, h.Machines.DeleteSnapshot(r.Context(), caller, r.PathValue("name"))
}

func (h *handler) events(r *http.Request, caller string) (int, any, error) {
	p, err := h.Machines.Events(r.Context(), caller, r.URL.Query().Get("since"))
	return http.StatusOK, p, err
}

func (h *handler) putAlias(r *http.Request, caller string) (int, any, error) {
	var a machineapi.AliasRequest
	if err := decode(r, &a); err != nil {
		return 0, nil, err
	}
	al, created, err := h.Machines.PutAlias(r.Context(), caller, r.PathValue("hostname"), a)
	return createdOr(created), al, err
}

func (h *handler) alias(r *http.Request, caller string) (int, any, error) {
	a, err := h.Machines.Alias(r.Context(), caller, r.PathValue("hostname"))
	return http.StatusOK, a, err
}

func (h *handler) deleteAlias(r *http.Request, caller string) (int, any, error) {
	return http.StatusNoContent, nil, h.Machines.DeleteAlias(r.Context(), caller, r.PathValue("hostname"))
}
