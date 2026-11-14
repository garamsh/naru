// Package wire converts a group spec between the engine's types and the API's, which is also the
// form the store keeps.
package wire

import (
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

// Spec is the engine's form of an API group spec.
func Spec(a orchapi.GroupSpec) groups.GroupSpec {
	g := groups.GroupSpec{
		Name: a.Name, Count: a.Count, Region: a.Region, RequiredLabels: a.RequiredLabels,
		Rollout:      groups.Rollout{MaxUnavailable: a.Rollout.MaxUnavailable, MaxSurge: a.Rollout.MaxSurge},
		MinAvailable: a.MinAvailable, Stateful: a.Stateful, FenceTTL: time.Duration(a.FenceTTLSeconds) * time.Second,
		Template: groups.Template{
			Form: groups.Form(a.Template.Form), CPUs: a.Template.CPUs, MemoryMiB: a.Template.MemoryMiB,
			Version: a.Template.Version, NodeVersion: a.Template.NodeVersion,
			Network: groups.NetworkPolicy{Egress: rules(a.Template.Network.Egress), Ingress: rules(a.Template.Network.Ingress)},
		},
	}
	for _, p := range a.PreferredLabels {
		g.PreferredLabels = append(g.PreferredLabels, groups.LabelPreference{Key: p.Key, Value: p.Value, Weight: p.Weight})
	}
	for _, d := range a.Template.Disks {
		g.Template.Disks = append(g.Template.Disks, groups.Disk{Name: d.Name, SizeBytes: d.SizeBytes, ReadOnly: d.ReadOnly})
	}
	return g
}

func rules(in []orchapi.GroupRule) []groups.Rule {
	var out []groups.Rule
	for _, r := range in {
		out = append(out, groups.Rule{Prefix: r.Prefix, Protocol: r.Protocol, Ports: r.Ports})
	}
	return out
}

// API is the API's form of an engine group spec.
func API(g groups.GroupSpec) orchapi.GroupSpec {
	a := orchapi.GroupSpec{
		Name: g.Name, Count: g.Count, Region: g.Region, RequiredLabels: g.RequiredLabels,
		Rollout:      orchapi.GroupRollout{MaxUnavailable: g.Rollout.MaxUnavailable, MaxSurge: g.Rollout.MaxSurge},
		MinAvailable: g.MinAvailable, Stateful: g.Stateful, FenceTTLSeconds: int(g.FenceTTL / time.Second),
		Template: orchapi.GroupTemplate{
			Form: string(g.Template.Form), CPUs: g.Template.CPUs, MemoryMiB: g.Template.MemoryMiB,
			Version: g.Template.Version, NodeVersion: g.Template.NodeVersion,
			Network: orchapi.GroupNetwork{Egress: apiRules(g.Template.Network.Egress), Ingress: apiRules(g.Template.Network.Ingress)},
		},
	}
	for _, p := range g.PreferredLabels {
		a.PreferredLabels = append(a.PreferredLabels, orchapi.PreferredLabel{Key: p.Key, Value: p.Value, Weight: p.Weight})
	}
	for _, d := range g.Template.Disks {
		a.Template.Disks = append(a.Template.Disks, orchapi.GroupDisk{Name: d.Name, SizeBytes: d.SizeBytes, ReadOnly: d.ReadOnly})
	}
	return a
}

func apiRules(in []groups.Rule) []orchapi.GroupRule {
	var out []orchapi.GroupRule
	for _, r := range in {
		out = append(out, orchapi.GroupRule{Prefix: r.Prefix, Protocol: r.Protocol, Ports: r.Ports})
	}
	return out
}
