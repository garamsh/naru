package ccm_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/node/internal/ccm"
)

func service(ns, name, uid string, ports ...v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "x"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
	}
}

func node(name string, addrs ...v1.NodeAddress) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1.NodeStatus{Addresses: addrs}}
}

func internal(ip string) v1.NodeAddress { return v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip} }

func TestTheExposureIDHoldsTheNamespaceTheNameAndTheUIDsFirstEightDigits(t *testing.T) {
	s := service("prod", "web", "")
	s.UID = "0a1b2c3d-1111-2222-3333-444455556666"
	require.Equal(t, "prod.web.0a1b2c3d", ccm.ExposureID(s))
	s.UID = "ffffffff-1111-2222-3333-444455556666"
	require.Equal(t, "prod.web.ffffffff", ccm.ExposureID(s), "a Service made again under the name is another exposure")
}

func TestTheSpecCarriesThePortsTheNodePortsAndOnlyIPv4InternalAddresses(t *testing.T) {
	s := service("d", "web", "", v1.ServicePort{Protocol: v1.ProtocolTCP, Port: 443, NodePort: 30443}, v1.ServicePort{Protocol: v1.ProtocolUDP, Port: 53, NodePort: 30053})
	s.Spec.LoadBalancerIP = "192.168.0.85"
	nodes := []*v1.Node{
		node("b", internal("10.1.0.6"), v1.NodeAddress{Type: v1.NodeExternalIP, Address: "203.0.113.9"}),
		node("a", internal("fd00::5"), internal("10.1.0.5")),
		node("dup", internal("10.1.0.5")),
	}
	spec, err := ccm.Spec(s, nodes, "lab", "lan")
	require.NoError(t, err)
	require.Equal(t, orchapi.ExposureSpec{
		Network: "lab", Pool: "lan", RequestedAddress: "192.168.0.85",
		Ports:    []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, BackendPort: 30443}, {Protocol: "UDP", Port: 53, BackendPort: 30053}},
		Backends: []string{"10.1.0.5", "10.1.0.6"},
	}, spec, "the IPv6 and external addresses and the duplicate are left out, the rest sorted")
}

func TestOnlyTheProxyProtocolV2AnnotationTurnsItOn(t *testing.T) {
	port := v1.ServicePort{Port: 443, NodePort: 30443}
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}
	for value, want := range map[string]string{"v2": "v2", "v1": "", "true": "", "": ""} {
		s := service("d", "web", "", port)
		s.Annotations = map[string]string{ccm.ProxyProtocolAnnotation: value}
		spec, err := ccm.Spec(s, nodes, "lab", "lan")
		require.NoError(t, err)
		require.Equal(t, want, spec.ProxyProtocol, "annotation %q", value)
	}
	spec, err := ccm.Spec(service("d", "web", "", port), nodes, "lab", "lan")
	require.NoError(t, err)
	require.Empty(t, spec.ProxyProtocol, "no annotation")
}

func TestTheSpecRefusesWhatOrchCannotCarry(t *testing.T) {
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}
	_, err := ccm.Spec(service("d", "web", "", v1.ServicePort{Protocol: v1.ProtocolSCTP, Port: 1, NodePort: 30001}), nodes, "lab", "lan")
	require.ErrorContains(t, err, "SCTP")
	_, err = ccm.Spec(service("d", "web", "", v1.ServicePort{Port: 80}), nodes, "lab", "lan")
	require.Error(t, err, "no NodePort yet")
	_, err = ccm.Spec(service("d", "web", "", v1.ServicePort{Port: 80, NodePort: 30080}), []*v1.Node{node("a")}, "lab", "lan")
	require.ErrorContains(t, err, "InternalIP")
	_, err = ccm.Spec(service("d", "web", "", v1.ServicePort{Port: 80, NodePort: 30080}), nodes, "lab", "lan")
	require.NoError(t, err, "control: the same Service with a NodePort and a Node")
}

func TestOrphansAreTheExposuresNoLoadBalancerServiceHas(t *testing.T) {
	keep := service("d", "keep", "")
	keep.UID = "aaaaaaaa-0000-0000-0000-000000000000"
	clusterIP := service("d", "cip", "")
	clusterIP.UID = "cccccccc-0000-0000-0000-000000000000"
	clusterIP.Spec.Type = v1.ServiceTypeClusterIP
	own := func(id string) orchapi.Exposure { return orchapi.Exposure{ID: id, Owner: orchapi.OwnerExposer} }
	exps := []orchapi.Exposure{own("d.keep.aaaaaaaa"), own("d.gone.dddddddd"), own("d.cip.cccccccc"), own("d.keep.eeeeeeee")}
	require.Equal(t,
		[]string{"d.gone.dddddddd", "d.cip.cccccccc", "d.keep.eeeeeeee"},
		ccm.Orphans(exps, []v1.Service{*keep, *clusterIP}),
		"a Service that is gone, one no longer a LoadBalancer, and one re-made under a new UID")
}

func TestOrphansLeaveTheExposuresTheOperatorDeclared(t *testing.T) {
	declared := orchapi.Exposure{ID: "api", Owner: orchapi.OwnerOperator}
	gone := orchapi.Exposure{ID: "d.gone.dddddddd", Owner: orchapi.OwnerExposer}
	require.Equal(t, []string{"d.gone.dddddddd"}, ccm.Orphans([]orchapi.Exposure{declared, gone}, nil),
		"no Service has the operator's exposure, and it is not released; the controller's own is")
	require.Equal(t, []string{"d.gone.dddddddd", "api"}, ccm.Orphans([]orchapi.Exposure{gone, {ID: "api", Owner: orchapi.OwnerExposer}}, nil),
		"control: the same id owned by the exposer is an orphan")
}
