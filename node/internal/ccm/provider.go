package ccm

import (
	"context"
	"io"
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
)

// SweepInterval is how often the controller lists its exposures and deletes those whose Service is
// gone, so an address is released even when the delete event was missed (ADR 0085 item 4).
const SweepInterval = time.Minute

// Provider is the naru cloud provider: the node controller always, the service controller unless
// the config turns it off.
type Provider struct {
	cfg  Config
	orch Orch
	log  *slog.Logger

	// events is set by Initialize, before the service controller asks for the LoadBalancer.
	events record.EventRecorder
}

var _ cloudprovider.Interface = (*Provider)(nil)

// New returns a Provider that calls orch.
func New(cfg Config, o Orch, log *slog.Logger) *Provider {
	return &Provider{cfg: cfg, orch: o, log: log}
}

// Register makes `--cloud-provider=naru` build a Provider from the cloud-config file.
func Register(log *slog.Logger) {
	cloudprovider.RegisterCloudProvider(ProviderName, func(r io.Reader) (cloudprovider.Interface, error) {
		cfg, err := ParseConfig(r)
		if err != nil {
			return nil, err
		}
		c, err := cfg.Client()
		if err != nil {
			return nil, err
		}
		return New(cfg, c, log), nil
	})
}

// Initialize starts the sweep that releases the addresses of Services that are gone.
func (p *Provider) Initialize(b cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	if !p.cfg.LoadBalancerOn() {
		return
	}
	kube := b.ClientOrDie("naru-ccm-sweep")
	ctx, cancel := context.WithCancel(context.Background())
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events(metav1.NamespaceAll)})
	p.events = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: ProviderName + "-ccm"})
	go func() { <-stop; cancel(); broadcaster.Shutdown() }()
	go func() {
		t := time.NewTicker(SweepInterval)
		defer t.Stop()
		for {
			err := p.Sweep(ctx, func(ctx context.Context) ([]v1.Service, error) {
				list, err := kube.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				return list.Items, nil
			})
			if err != nil && ctx.Err() == nil {
				p.log.Error("sweep failed", "err", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Sweep deletes the exposures of the network that no Service has. It lists the exposures first and
// the Services after: an exposure exists only once its Service does, so a Service made between the two
// calls is in the second list or its exposure is not in the first, and its address is not released.
func (p *Provider) Sweep(ctx context.Context, listServices func(context.Context) ([]v1.Service, error)) error {
	have, err := p.orch.Exposures(ctx)
	if err != nil {
		return err
	}
	services, err := listServices(ctx)
	if err != nil {
		return err
	}
	for _, id := range Orphans(have, services) {
		if err := p.orch.DeleteExposure(ctx, id); err != nil {
			return err
		}
		p.log.Info("released the address of a Service that is gone", "exposure", id)
	}
	return nil
}

// LoadBalancer is the service controller's side, unless it is off.
func (p *Provider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if !p.cfg.LoadBalancerOn() {
		return nil, false
	}
	return NewLoadBalancer(p.orch, p.cfg.Network, p.cfg.Pool, p.events), true
}

// InstancesV2 is the node controller's side.
func (p *Provider) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return NewInstances(p.orch, p.cfg.Network), true
}

// Instances is not served: InstancesV2 is.
func (p *Provider) Instances() (cloudprovider.Instances, bool) { return nil, false }

// Zones is not served: InstancesV2 carries the zone.
func (p *Provider) Zones() (cloudprovider.Zones, bool) { return nil, false }

// Clusters is not served.
func (p *Provider) Clusters() (cloudprovider.Clusters, bool) { return nil, false }

// Routes is not served: the Pod networks are the hosts'.
func (p *Provider) Routes() (cloudprovider.Routes, bool) { return nil, false }

// ProviderName is "naru".
func (p *Provider) ProviderName() string { return ProviderName }

// HasClusterID is false: orch's network is the cluster's identity.
func (p *Provider) HasClusterID() bool { return false }
