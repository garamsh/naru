package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// HostInfo is spec.host as init writes it to the host file.
type HostInfo struct {
	Region string            `json:"region"`
	Labels map[string]string `json:"labels"`
}

// readInit returns what init wrote of itself, and nil when the file is absent: a host whose init
// has not written it has no version to report, which is not a failure of the request.
func (s *Server) readInit() (*hostapi.NaruInfo, error) {
	if s.deps.InitFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.deps.InitFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info hostapi.NaruInfo
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.deps.HostFile)
	var info HostInfo
	if err == nil {
		err = json.Unmarshal(b, &info)
	}
	if err != nil {
		s.deps.Log.Error("cannot read the host file", "file", s.deps.HostFile, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "the host's region and labels cannot be read"})
		return
	}
	if info.Labels == nil {
		info.Labels = map[string]string{}
	}
	naru, err := s.readInit()
	if err != nil {
		s.deps.Log.Error("cannot read the init file", "file", s.deps.InitFile, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "the host's naru version cannot be read"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rt, err := s.deps.Runtime.Info(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	machines, err := s.deps.Runtime.ListMachines(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := hostapi.Host{
		HostID: s.deps.HostID, Region: info.Region, Labels: info.Labels,
		Capacity: hostapi.Resources{CPU: rt.CPUs, Memory: rt.MemoryBytes, Disk: rt.StateTotalBytes},
		Mvmd:     hostapi.MvmdInfo{Version: rt.APIVersion, Driver: rt.Driver, Kernels: rt.Kernels},
		Naru:     naru,
	}
	if out.Mvmd.Kernels == nil {
		out.Mvmd.Kernels = []mvmd.Kernel{}
	}
	out.Free = hostapi.Resources{CPU: rt.CPUs, Memory: rt.MemoryBytes, Disk: rt.StateAvailableBytes}
	for _, m := range machines {
		if m.State != mvmd.StateRunning && m.State != mvmd.StatePaused {
			continue
		}
		out.Free.CPU = out.Free.CPU - min(out.Free.CPU, m.CPU)
		out.Free.Memory -= min(out.Free.Memory, m.MemoryBytes)
	}
	writeJSON(w, http.StatusOK, out)
}
