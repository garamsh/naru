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
	"github.com/garamsh/naru/lib/identity"
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
	// A reported label is the host's alone: one the host file names, as a configuration from before
	// the label was reported may, is not the operator's to set.
	reported := map[string]string{}
	if s.deps.Virtualization != "" {
		reported[hostapi.LabelVirtualization] = s.deps.Virtualization
	}
	for _, key := range hostapi.ReportedLabels {
		delete(info.Labels, key)
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
		HostID: s.deps.HostID, LANInterface: s.deps.LANInterface, Region: info.Region, Labels: info.Labels, ReportedLabels: reported,
		Capacity: hostapi.Resources{CPU: rt.CPUs, Memory: rt.MemoryBytes, Disk: rt.StateTotalBytes},
		Mvmd:     hostapi.MvmdInfo{Version: rt.APIVersion, Driver: rt.Driver, Kernels: rt.Kernels},
		Naru:     naru,
	}
	if out.Mvmd.Kernels == nil {
		out.Mvmd.Kernels = []mvmd.Kernel{}
	}
	if s.deps.ProxyHealthy != nil {
		out.Proxy = &hostapi.ProxyStatus{Healthy: s.deps.ProxyHealthy(ctx)}
		if s.deps.Routes != nil {
			out.Proxy.RouteMaxAgeSeconds = int64((s.deps.Routes.MaxAge + time.Second - 1) / time.Second)
		}
	}
	out.TrustExpiresAt = s.trustExpiry()
	out.Mvmd.BootSets = hostBootSets(rt.BootSets)
	out.Mvmd.MachinePrefixes = append([]string{}, rt.MachinePrefixes...)
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

// trustExpiry is when the client CA pool stops trusting an authority it holds, read from the file
// on each call, so a refreshed pool reports at once; nil when the pool cannot be read or holds none.
func (s *Server) trustExpiry() *time.Time {
	if s.deps.ClientCAFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.deps.ClientCAFile)
	if err != nil {
		s.deps.Log.Error("cannot read the client CA pool", "file", s.deps.ClientCAFile, "err", err.Error())
		return nil
	}
	at, ok := identity.TrustExpiry(b)
	if !ok {
		return nil
	}
	at = at.UTC()
	return &at
}

// hostBootSets is the boot sets mvmd offers as the host API says them; an empty list is [], not null.
func hostBootSets(in []mvmd.BootSet) []hostapi.BootSet {
	out := make([]hostapi.BootSet, 0, len(in))
	for _, b := range in {
		out = append(out, hostapi.BootSet{Name: b.Name, KernelSHA256: b.KernelDigest, InitramfsSHA256: b.InitrdDigest, Origin: b.Origin})
	}
	return out
}
