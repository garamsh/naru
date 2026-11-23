package ccm

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"

	"github.com/garamsh/naru/lib/orchapi"
)

const (
	// ProviderName is the name the controller registers under and the scheme of a Node's provider ID.
	ProviderName = "naru"
	// LoadBalancerClass is the class of a Service this controller fulfils besides one with none.
	LoadBalancerClass = "naru.garam.sh/host"
	// ProxyProtocolAnnotation asks for the client's address in a PROXY protocol header; "v2" is the
	// one value, and any other is off.
	ProxyProtocolAnnotation = "naru.garam.sh/proxy-protocol"
)

// errNodePort is a Service whose NodePorts Kubernetes has not allocated yet; the controller asks again.
var errNodePort = errors.New("a port of the Service has no NodePort yet")

// Wants reports whether the controller fulfils the Service: a Service of any other class belongs to
// the implementation that owns it.
func Wants(svc *v1.Service) bool {
	c := svc.Spec.LoadBalancerClass
	return c == nil || *c == "" || *c == LoadBalancerClass
}

// ExposureID is the exposure's id for the Service: its namespace and name and its UID's first eight
// hex digits, so a Service deleted and made again is a new exposure.
func ExposureID(svc *v1.Service) string {
	uid := strings.ReplaceAll(string(svc.UID), "-", "")
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return svc.Namespace + "." + svc.Name + "." + uid
}

// Spec is what the controller asks orch for one Service: its ports with their NodePorts and the
// InternalIP of each Node as a backend.
func Spec(svc *v1.Service, nodes []*v1.Node, network, pool string) (orchapi.ExposureSpec, error) {
	spec := orchapi.ExposureSpec{Network: network, Pool: pool, RequestedAddress: svc.Spec.LoadBalancerIP}
	for _, p := range svc.Spec.Ports {
		var proto string
		switch p.Protocol {
		case v1.ProtocolTCP, "":
			proto = "TCP"
		case v1.ProtocolUDP:
			proto = "UDP"
		default:
			return spec, fmt.Errorf("port %d is %s: only TCP and UDP are carried", p.Port, p.Protocol)
		}
		if p.NodePort == 0 {
			return spec, errNodePort
		}
		spec.Ports = append(spec.Ports, orchapi.ExposurePort{Protocol: proto, Port: int(p.Port), NodePort: int(p.NodePort)})
	}
	spec.Backends = Backends(nodes)
	if len(spec.Backends) == 0 {
		return spec, errors.New("no Node has an IPv4 InternalIP")
	}
	if svc.Annotations[ProxyProtocolAnnotation] == orchapi.ProxyProtocolV2 {
		spec.ProxyProtocol = orchapi.ProxyProtocolV2
	}
	return spec, nil
}

// Backends is the sorted, distinct IPv4 InternalIPs of the Nodes.
func Backends(nodes []*v1.Node) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			ip, err := netip.ParseAddr(a.Address)
			if a.Type == v1.NodeInternalIP && err == nil && ip.Is4() && !seen[a.Address] {
				seen[a.Address] = true
				out = append(out, a.Address)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Status is the Service's load-balancer status: the address orch assigned.
func Status(e orchapi.Exposure) *v1.LoadBalancerStatus {
	return &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: e.Address}}}
}

// Orphans returns the ids of the exposures no Service the controller fulfils still has. A Service
// that is being deleted still has its exposure: the finalizer's removal calls the delete.
func Orphans(exposures []orchapi.Exposure, services []v1.Service) []string {
	live := map[string]bool{}
	for i := range services {
		s := &services[i]
		if s.Spec.Type == v1.ServiceTypeLoadBalancer && Wants(s) {
			live[ExposureID(s)] = true
		}
	}
	var out []string
	for _, e := range exposures {
		if !live[e.ID] {
			out = append(out, e.ID)
		}
	}
	return out
}
