package api

import (
	"context"
	"encoding/json"
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
		Region: info.Region, Labels: info.Labels,
		Capacity: hostapi.Resources{CPU: rt.CPUs, Memory: rt.MemoryBytes, Disk: rt.StateTotalBytes},
		Mvmd:     hostapi.MvmdInfo{Version: rt.APIVersion, Driver: rt.Driver, Kernels: rt.Kernels},
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
