package ccm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/api"

	"github.com/garamsh/naru/lib/orchapi"
)

// retryAfter is how soon the service controller asks again while an exposure is not yet announced
// or a NodePort not yet allocated.
const retryAfter = 5 * time.Second

// LoadBalancer fulfils a Service of type LoadBalancer with an exposure of orch.
type LoadBalancer struct {
	orch    Orch
	network string
	pool    string
}

var _ cloudprovider.LoadBalancer = (*LoadBalancer)(nil)

// NewLoadBalancer returns the service controller's side of the provider for one network and pool.
func NewLoadBalancer(o Orch, network, pool string) *LoadBalancer {
	return &LoadBalancer{orch: o, network: network, pool: pool}
}

// notFound reports whether orch answered that there is no such exposure.
func notFound(err error) bool {
	var e *orchapi.Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// GetLoadBalancer returns the exposure's address while orch has announced it.
func (l *LoadBalancer) GetLoadBalancer(ctx context.Context, _ string, svc *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	e, err := l.orch.Exposure(ctx, ExposureID(svc))
	if notFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return Status(e), true, nil
}

// GetLoadBalancerName is the exposure's id.
func (l *LoadBalancer) GetLoadBalancerName(_ context.Context, _ string, svc *v1.Service) string {
	return ExposureID(svc)
}

// EnsureLoadBalancer asks orch for the exposure and returns its address once a host announces it.
func (l *LoadBalancer) EnsureLoadBalancer(ctx context.Context, _ string, svc *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	spec, err := Spec(svc, nodes, l.network, l.pool)
	if errors.Is(err, errNodePort) {
		return nil, api.NewRetryError(err.Error(), retryAfter)
	}
	if err != nil {
		return nil, err
	}
	e, err := l.orch.ApplyExposure(ctx, ExposureID(svc), spec)
	if err != nil {
		return nil, fmt.Errorf("exposure %s: %w", ExposureID(svc), err)
	}
	if e.Phase != orchapi.ExposureAnnounced {
		return nil, api.NewRetryError(fmt.Sprintf("exposure %s has address %s and no host announces it yet", e.ID, e.Address), retryAfter)
	}
	return Status(e), nil
}

// UpdateLoadBalancer changes the exposure's backends to the Nodes' as they are now.
func (l *LoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, svc *v1.Service, nodes []*v1.Node) error {
	_, err := l.EnsureLoadBalancer(ctx, clusterName, svc, nodes)
	var retry *api.RetryError
	if errors.As(err, &retry) {
		return nil // the spec is stored; only the announcement is awaited
	}
	return err
}

// EnsureLoadBalancerDeleted starts the release of the Service's address; it is idempotent.
func (l *LoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, _ string, svc *v1.Service) error {
	return l.orch.DeleteExposure(ctx, ExposureID(svc))
}
