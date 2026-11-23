package ccm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/garamsh/naru/lib/orchapi"
)

// Instances is the node controller's side of the provider: a Node is a member of a group on the
// network, and its name is the member's name.
type Instances struct {
	orch    Orch
	network string
}

var _ cloudprovider.InstancesV2 = (*Instances)(nil)

// NewInstances returns the node controller's side of the provider for one network.
func NewInstances(o Orch, network string) *Instances { return &Instances{orch: o, network: network} }

// ProviderID is a member's provider ID, naru://<host-id>/<member-name>.
func ProviderID(m orchapi.NetworkMember) string { return ProviderName + "://" + m.Host + "/" + m.Name }

// memberName is the member a Node is: the name in its provider ID when it has one, else its own name.
func memberName(n *v1.Node) (string, error) {
	id := n.Spec.ProviderID
	if id == "" {
		return n.Name, nil
	}
	rest, ok := strings.CutPrefix(id, ProviderName+"://")
	_, name, two := strings.Cut(rest, "/")
	if !ok || !two || name == "" {
		return "", fmt.Errorf("provider ID %q is not naru://<host>/<member>", id)
	}
	return name, nil
}

// find returns the member the Node is, and whether it exists. A member gone from its group is not
// listed, and one whose host did not answer is, so a Node that is only away keeps existing.
func (i *Instances) find(ctx context.Context, n *v1.Node) (orchapi.NetworkMember, bool, error) {
	name, err := memberName(n)
	if err != nil {
		return orchapi.NetworkMember{}, false, err
	}
	ms, err := i.orch.NetworkMembers(ctx, i.network)
	if err != nil {
		return orchapi.NetworkMember{}, false, err
	}
	for _, m := range ms {
		if m.Name == name {
			return m, true, nil
		}
	}
	return orchapi.NetworkMember{}, false, nil
}

// InstanceExists is false only for a Node whose member orch no longer lists, never for one whose
// host merely does not answer or for an orch that cannot be asked.
func (i *Instances) InstanceExists(ctx context.Context, n *v1.Node) (bool, error) {
	_, ok, err := i.find(ctx, n)
	return ok, err
}

// InstanceShutdown is true for a member a host reported and that is not running; a member no host
// reported is away, not shut down.
func (i *Instances) InstanceShutdown(ctx context.Context, n *v1.Node) (bool, error) {
	m, ok, err := i.find(ctx, n)
	if err != nil {
		return false, err
	}
	return ok && m.Reported && !m.Running, nil
}

// InstanceMetadata names the Node's provider ID, address, region and zone.
func (i *Instances) InstanceMetadata(ctx context.Context, n *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	m, ok, err := i.find(ctx, n)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, cloudprovider.InstanceNotFound
	}
	if m.Address == "" {
		return nil, errors.New("member " + m.Name + " has no address: its host did not report it")
	}
	return &cloudprovider.InstanceMetadata{
		ProviderID:    ProviderID(m),
		NodeAddresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: m.Address}},
		Region:        m.Region,
		Zone:          m.Zone,
	}, nil
}
