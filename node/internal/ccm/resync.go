package ccm

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// resyncDelay gathers the slice changes of one Service into one pass.
const resyncDelay = time.Second

// Resyncer stores the backends of a Service that sets externalTrafficPolicy Local again when its
// EndpointSlices change: the service controller calls the provider on a change of the Service or of the
// Nodes, and a Pod that moves changes neither.
type Resyncer struct {
	lb   *LoadBalancer
	kube kubernetes.Interface
}

// NewResyncer returns a Resyncer that updates through lb.
func NewResyncer(lb *LoadBalancer, kube kubernetes.Interface) *Resyncer {
	return &Resyncer{lb: lb, kube: kube}
}

// Sync updates the exposure of the named Service with the Nodes the service controller last passed for it. A Service that is gone,
// is not of type LoadBalancer or does not set Local is not its business.
func (r *Resyncer) Sync(ctx context.Context, namespace, name string) error {
	svc, err := r.kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer || !wantsLocal(svc) || svc.DeletionTimestamp != nil {
		return nil
	}
	return r.lb.Resync(ctx, svc)
}

// Run watches the EndpointSlices and syncs the Service each change names, until ctx ends.
func (r *Resyncer) Run(ctx context.Context, onError func(key string, err error)) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()
	enqueue := func(obj any) {
		if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = t.Obj
		}
		if s, ok := obj.(*discoveryv1.EndpointSlice); ok && s.Labels[discoveryv1.LabelServiceName] != "" {
			queue.AddAfter(s.Namespace+"/"+s.Labels[discoveryv1.LabelServiceName], resyncDelay)
		}
	}
	factory := informers.NewSharedInformerFactory(r.kube, 0)
	_, err := factory.Discovery().V1().EndpointSlices().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		DeleteFunc: enqueue,
	})
	if err != nil {
		onError("", err)
		return
	}
	factory.Start(ctx.Done())
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}
		ns, name, _ := cache.SplitMetaNamespaceKey(key)
		if err := r.Sync(ctx, ns, name); err != nil {
			onError(key, err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}
		queue.Done(key)
	}
}
