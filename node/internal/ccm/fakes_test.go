package ccm_test

import (
	"context"
	"net/http"
	"sort"
	"sync"

	"github.com/garamsh/naru/lib/orchapi"
)

// fakeOrch is orch as the controller sees it: it keeps the exposures it was asked for and lists
// the members it was given.
type fakeOrch struct {
	mu       sync.Mutex
	exps     map[string]orchapi.Exposure
	members  []orchapi.NetworkMember
	phase    string
	err      error
	applied  []orchapi.ExposureSpec
	deletes  []string
	memberOf string
}

func newFakeOrch() *fakeOrch {
	return &fakeOrch{exps: map[string]orchapi.Exposure{}, phase: orchapi.ExposureAnnounced}
}

func (f *fakeOrch) ApplyExposure(_ context.Context, id string, spec orchapi.ExposureSpec) (orchapi.Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return orchapi.Exposure{}, f.err
	}
	f.applied = append(f.applied, spec)
	e := orchapi.Exposure{ID: id, Network: spec.Network, Pool: spec.Pool, Address: "192.168.0.80", Ports: spec.Ports, Backends: spec.Backends, Phase: f.phase}
	f.exps[id] = e
	return e, nil
}

func (f *fakeOrch) Exposure(_ context.Context, id string) (orchapi.Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return orchapi.Exposure{}, f.err
	}
	e, ok := f.exps[id]
	if !ok {
		return e, &orchapi.Error{Status: http.StatusNotFound, Message: "not found"}
	}
	return e, nil
}

func (f *fakeOrch) Exposures(context.Context) ([]orchapi.Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orchapi.Exposure
	for _, e := range f.exps {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, f.err
}

func (f *fakeOrch) DeleteExposure(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	delete(f.exps, id)
	return f.err
}

func (f *fakeOrch) NetworkMembers(_ context.Context, name string) ([]orchapi.NetworkMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memberOf = name
	return f.members, f.err
}
