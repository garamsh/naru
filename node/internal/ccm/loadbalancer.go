package ccm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
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
	events  record.EventRecorder
	ends    Endpoints

	mu    sync.Mutex
	nodes map[string][]*v1.Node // per exposure, the Nodes the service controller last passed
}

var _ cloudprovider.LoadBalancer = (*LoadBalancer)(nil)

// NewLoadBalancer returns the service controller's side of the provider for one network and pool.
// The recorder carries the events a Service is told; nil tells nobody. ends says where the endpoints
// of a Service that sets Local run.
func NewLoadBalancer(o Orch, network, pool string, events record.EventRecorder, ends Endpoints) *LoadBalancer {
	return &LoadBalancer{orch: o, network: network, pool: pool, events: events, ends: ends, nodes: map[string][]*v1.Node{}}
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
	l.notify(svc)
	l.remember(svc, nodes)
	picked, err := SelectNodes(ctx, svc, nodes, l.ends)
	if err != nil {
		return nil, err
	}
	spec, err := Spec(svc, picked, l.network, l.pool)
	if errors.Is(err, errNodePort) || errors.Is(err, errNoBackend) {
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

// remember keeps the Nodes the service controller passed for the Service, for Resync.
func (l *LoadBalancer) remember(svc *v1.Service, nodes []*v1.Node) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nodes[ExposureID(svc)] = nodes
}

// Resync stores the exposure again, through the same stages, with the Nodes the service controller
// last passed for the Service. It does nothing before the controller has passed any, which it does for
// every Service when it starts.
func (l *LoadBalancer) Resync(ctx context.Context, svc *v1.Service) error {
	l.mu.Lock()
	nodes, ok := l.nodes[ExposureID(svc)]
	l.mu.Unlock()
	if !ok {
		return nil
	}
	return l.UpdateLoadBalancer(ctx, "", svc, nodes)
}

// notify tells the Service what its exposure is not: an event for each notice its spec earns.
// The recorder folds a repeated event into a count, so every pass of the controller may call it.
func (l *LoadBalancer) notify(svc *v1.Service) {
	if l.events == nil {
		return
	}
	if reason, message, ok := LocalPolicyNotice(svc); ok {
		l.events.Event(svc, v1.EventTypeNormal, reason, message)
	}
}

// UpdateLoadBalancer changes the exposure's backends to the Nodes' as they are now. A wait
// for the announcement leaves the spec stored; a missing NodePort or backend leaves nothing stored.
func (l *LoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, svc *v1.Service, nodes []*v1.Node) error {
	_, err := l.EnsureLoadBalancer(ctx, clusterName, svc, nodes)
	var retry *api.RetryError
	if errors.As(err, &retry) {
		return nil // a retry waits for the announcement, a NodePort or a backend; it is not a failure
	}
	return err
}

// EnsureLoadBalancerDeleted starts the release of the Service's address; it is idempotent.
func (l *LoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, _ string, svc *v1.Service) error {
	l.mu.Lock()
	delete(l.nodes, ExposureID(svc))
	l.mu.Unlock()
	return l.orch.DeleteExposure(ctx, ExposureID(svc))
}
