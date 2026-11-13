// Package declaration turns a NaruClusterDeclaration into the Cluster API objects of one cluster
// and keeps them matching it.
package declaration

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
)

// Finalizer holds a declaration until its Cluster is gone. Owner references alone make the
// garbage collector delete the Cluster, but only the finalizer lets the declaration say it is gone
// when the Cluster really is, and makes the deletion work where no garbage collector runs.
const Finalizer = "naru.garam.sh/cluster"

// Reconciler creates and updates the objects of a declaration, each owned by it.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the reconciler for NaruClusterDeclaration and for every kind it
// creates. name is the controller's name, which must be unique in a process.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).Named(name).
		For(&v1alpha1.NaruClusterDeclaration{}).
		Owns(&clusterv1.Cluster{}).
		Owns(&v1alpha1.NaruCluster{}).
		Owns(&v1alpha1.NaruControlPlane{}).
		Owns(&v1alpha1.NaruMachineTemplate{}).
		Owns(&clusterv1.MachineDeployment{}).
		Owns(&v1alpha1.NaruConfigTemplate{}).
		Complete(r)
}

// Reconcile makes the cluster's objects match the declaration, or, once the declaration is
// deleted, deletes the Cluster and releases the declaration when the Cluster is gone.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var d v1alpha1.NaruClusterDeclaration
	if err := r.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !d.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &d)
	}
	if controllerutil.AddFinalizer(&d, Finalizer) {
		if err := r.Update(ctx, &d); err != nil {
			return ctrl.Result{}, err
		}
	}

	applyErr := r.applyAll(ctx, &d)
	cond := metav1.Condition{
		Type: v1alpha1.DeclarationReconciled, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonApplied,
		Message: "the Cluster API objects match the declaration", ObservedGeneration: d.Generation,
	}
	if applyErr != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, applyErr.Error()
	}
	before := d.Status.DeepCopy()
	d.Status.ObservedGeneration = d.Generation
	meta.SetStatusCondition(&d.Status.Conditions, cond)
	if before.ObservedGeneration != d.Status.ObservedGeneration || !conditionsEqual(before.Conditions, d.Status.Conditions) {
		if err := r.Status().Update(ctx, &d); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, applyErr
}

// applyAll creates or updates the seven objects, the ones other objects refer to first.
func (r *Reconciler) applyAll(ctx context.Context, d *v1alpha1.NaruClusterDeclaration) error {
	ns := d.Namespace
	objs := []client.Object{
		&v1alpha1.NaruCluster{ObjectMeta: named(ns, d.Name)},
		&v1alpha1.NaruMachineTemplate{ObjectMeta: named(ns, controlPlaneTemplateName(d.Name))},
		&v1alpha1.NaruControlPlane{ObjectMeta: named(ns, d.Name)},
		&clusterv1.Cluster{ObjectMeta: named(ns, d.Name)},
		&v1alpha1.NaruMachineTemplate{ObjectMeta: named(ns, workersName(d.Name))},
		&v1alpha1.NaruConfigTemplate{ObjectMeta: named(ns, workersName(d.Name))},
		&clusterv1.MachineDeployment{ObjectMeta: named(ns, workersName(d.Name))},
	}
	for _, obj := range objs {
		if err := r.apply(ctx, d, obj); err != nil {
			return fmt.Errorf("%T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// apply creates obj or updates it and makes d its controller. An object of that name that d does
// not control is an error: the declaration never takes over someone else's object.
func (r *Reconciler) apply(ctx context.Context, d *v1alpha1.NaruClusterDeclaration, obj client.Object) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		exists := !obj.GetCreationTimestamp().Time.IsZero()
		if exists && !metav1.IsControlledBy(obj, d) {
			return fmt.Errorf("it exists and the declaration does not control it")
		}
		fill(obj, d)
		return controllerutil.SetControllerReference(d, obj, r.Scheme())
	})
	return err
}

// finalize deletes the Cluster the declaration controls, and releases the declaration once the
// Cluster is gone. Cluster API's own deletion then takes down the rest.
func (r *Reconciler) finalize(ctx context.Context, d *v1alpha1.NaruClusterDeclaration) error {
	if !controllerutil.ContainsFinalizer(d, Finalizer) {
		return nil
	}
	var c clusterv1.Cluster
	err := r.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Name}, &c)
	switch {
	case err == nil && metav1.IsControlledBy(&c, d):
		if c.DeletionTimestamp == nil {
			if err := r.Delete(ctx, &c); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		// The Cluster's deletion event runs this again.
		return nil
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	controllerutil.RemoveFinalizer(d, Finalizer)
	return r.Update(ctx, d)
}

func named(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status || a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message || a[i].ObservedGeneration != b[i].ObservedGeneration {
			return false
		}
	}
	return true
}
