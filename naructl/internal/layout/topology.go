package layout

import (
	"encoding/json"
	"slices"
)

// Service is one of the services a Node can run besides mvmd, which a Node cannot do without.
type Service struct {
	Name string `json:"name"`
	// ControlPlane is set for a service only the control-plane Node runs.
	ControlPlane bool `json:"controlPlane"`
	// Gaon is set for a service that is one of gaon's binaries.
	Gaon bool `json:"gaon"`
}

// Topology is what a Node runs and who signs: the one definition of the service list, of which
// services the control-plane Node alone runs, and of the service that signs the agents' requests
// (ADR 0019). tooling/qemu reads it from `naructl topology`.
type Topology struct {
	// Services are in the order init starts them.
	Services []Service `json:"services"`
	// Signer is the service that signs the agents' requests on a control-plane Node that issues its
	// certificates; that Node cannot leave it out.
	Signer string `json:"signer"`
	// APIServer and Agent are the services a harness needs both of to ask the API whether the Nodes
	// have registered: the one that serves it and the one that registers a Node.
	APIServer string `json:"apiserver"`
	Agent     string `json:"agent"`
}

// Gaon is the Topology of a gaon Node.
var Gaon = Topology{
	Services: []Service{
		{Name: "registry"},
		{Name: "etcd", ControlPlane: true},
		{Name: "gaon-apiserver", ControlPlane: true, Gaon: true},
		{Name: "gaon-scheduler", ControlPlane: true, Gaon: true},
		{Name: "gaon-controllers", ControlPlane: true, Gaon: true},
		{Name: "gaon-node-agent", Gaon: true},
	},
	Signer:    "gaon-controllers",
	APIServer: "gaon-apiserver",
	Agent:     "gaon-node-agent",
}

// Names are the services' names, in order. A Node's configuration can leave out (Node.Disable) or
// give another binary (Node.Binaries) any of them.
func (t Topology) Names() []string {
	names := make([]string, len(t.Services))
	for i, s := range t.Services {
		names[i] = s.Name
	}
	return names
}

// Has reports whether name is one of the services.
func (t Topology) Has(name string) bool {
	return slices.ContainsFunc(t.Services, func(s Service) bool { return s.Name == name })
}

// Running are the services a Node runs when it is a control-plane Node or not, in order.
func (t Topology) Running(controlPlane bool) []Service {
	return slices.DeleteFunc(slices.Clone(t.Services), func(s Service) bool { return s.ControlPlane && !controlPlane })
}

// OnlyControlPlane reports whether name is a service the control-plane Node alone runs.
func (t Topology) OnlyControlPlane(name string) bool {
	return slices.ContainsFunc(t.Services, func(s Service) bool { return s.Name == name && s.ControlPlane })
}

// JSON is the Topology as `naructl topology` prints it.
func (t Topology) JSON() ([]byte, error) { return json.MarshalIndent(t, "", "  ") }
