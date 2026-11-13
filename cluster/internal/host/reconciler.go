package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
)

// Reconciler writes what a host's API answers into its NaruHost's status.
type Reconciler struct {
	client.Client
	// Interval is how long after an answer, or the lack of one, the host is asked again.
	Interval time.Duration
}

// SetupWithManager registers the reconciler for NaruHost. name is the controller's name, which
// must be unique in a process.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).Named(name).For(&v1alpha1.NaruHost{}).Complete(r)
}

// Reconcile asks the host and records the answer. A host that does not answer is a state to record,
// not an error to retry with back-off: the host is asked again after Interval either way.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var h v1alpha1.NaruHost
	if err := r.Get(ctx, req.NamespacedName, &h); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want := h.Status.DeepCopy()

	cond := metav1.Condition{Type: v1alpha1.HostReady, ObservedGeneration: h.Generation}
	pair, err := r.pair(ctx, h.Spec.ClientSecretRef)
	var info Info
	if err == nil {
		info, err = Fetch(ctx, h.Spec.Address, pair)
	}
	switch {
	case err == nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, v1alpha1.ReasonAnswered, "the host API answered"
		want.Region, want.Labels = info.Region, info.Labels
		want.Capacity, want.Free = quantities(info.Capacity), quantities(info.Free)
	case errors.Is(err, ErrRejected):
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonRejected, err.Error()
	case errors.Is(err, ErrClientPair):
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonClientPairInvalid, err.Error()
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonUnreachable, err.Error()
	}
	meta.SetStatusCondition(&want.Conditions, cond)

	if !apiequality.Semantic.DeepEqual(&h.Status, want) {
		h.Status = *want
		if err := r.Status().Update(ctx, &h); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: r.Interval}, nil
}

// pair reads the client pair and the CA from the Secret.
func (r *Reconciler) pair(ctx context.Context, ref v1alpha1.SecretReference) (Pair, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return Pair{}, fmt.Errorf("%w: Secret %s/%s does not exist", ErrClientPair, ref.Namespace, ref.Name)
		}
		return Pair{}, err
	}
	return Pair{Cert: s.Data[corev1.TLSCertKey], Key: s.Data[corev1.TLSPrivateKeyKey], CA: s.Data["ca.crt"]}, nil
}

func quantities(r Resources) v1alpha1.HostResources {
	return v1alpha1.HostResources{
		CPU:    *resource.NewQuantity(int64(r.CPU), resource.DecimalSI),
		Memory: *resource.NewQuantity(int64(r.Memory), resource.BinarySI),
		Disk:   *resource.NewQuantity(int64(r.Disk), resource.BinarySI),
	}
}
