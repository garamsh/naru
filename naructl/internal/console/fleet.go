package console

import (
	"cmp"
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// FleetSource is what the Fleet reads of orch's API. *orchapi.Client implements it.
type FleetSource interface {
	MicroVMs(ctx context.Context) ([]orchapi.MicroVM, error)
	Groups(ctx context.Context) ([]orchapi.Group, error)
}

type fleet struct{ src FleetSource }

// NewFleet returns the Fleet that reads microVMs and groups from orch's API. A microVM's image is
// the image digest orch reports, or the boot set for an OS-form microVM.
func NewFleet(src FleetSource) Fleet { return fleet{src} }

func (f fleet) MicroVMs(ctx context.Context, hostID string) ([]MicroVM, error) {
	all, err := f.src.MicroVMs(ctx)
	if err != nil {
		return nil, err
	}
	var out []MicroVM
	for _, m := range all {
		if m.Host == hostID {
			out = append(out, MicroVM{Name: m.Name, State: m.State, Group: m.Group, Image: cmp.Or(m.Image, m.BootSet), Network: m.Network})
		}
	}
	return out, nil
}

func (f fleet) Groups(ctx context.Context) ([]Group, error) {
	all, err := f.src.Groups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(all))
	for _, g := range all {
		ready := 0
		for _, m := range g.Members {
			if m.Phase == orchapi.PhaseRunning {
				ready++
			}
		}
		out = append(out, Group{Name: g.Spec.Name, Count: g.Spec.Count, Ready: ready, Stateful: g.Spec.Stateful})
	}
	return out, nil
}
