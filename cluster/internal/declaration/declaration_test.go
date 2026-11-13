package declaration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
	"github.com/garamsh/naru/cluster/internal/declaration"
	"github.com/garamsh/naru/cluster/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

// run starts the declaration controller on the shared API server and returns a client and a
// namespace of the test's own.
func run(t *testing.T) (client.Client, string) {
	t.Helper()
	mgr, start := testenv.Manager(t)
	require.NoError(t, (&declaration.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr, "declaration"))
	start()
	c := testenv.Client(t)
	return c, testenv.Namespace(t, c)
}

func declare(ns, name string) *v1alpha1.NaruClusterDeclaration {
	return &v1alpha1.NaruClusterDeclaration{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.NaruClusterDeclarationSpec{
			Region:       "eu",
			HostSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "gold"}},
			ControlPlane: v1alpha1.ControlPlaneDeclaration{Replicas: 3},
			Workers:      v1alpha1.WorkersDeclaration{Replicas: 2},
			Version:      "v1.31.0",
			Node:         v1alpha1.NodeDeclaration{CPU: 2, Memory: resource.MustParse("2Gi")},
		},
	}
}

func key(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

// eventually waits for cond, which reads the API server, to hold.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 20*time.Second, 50*time.Millisecond, msg)
}

// exists reports whether the object is on the API server.
func exists(c client.Client, k types.NamespacedName, obj client.Object) bool {
	return c.Get(context.Background(), k, obj) == nil
}

func requireControlledBy(t *testing.T, owner, obj client.Object) {
	t.Helper()
	ref := metav1.GetControllerOf(obj)
	require.NotNil(t, ref, "%T %s has no controller", obj, obj.GetName())
	require.Equal(t, owner.GetUID(), ref.UID, "%T %s", obj, obj.GetName())
	require.Equal(t, "NaruClusterDeclaration", ref.Kind)
}

func TestDeclaration_CreatesTheObjectsOwnedByIt(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	d := declare(ns, "prod")
	require.NoError(t, c.Create(ctx, d))

	var md clusterv1.MachineDeployment
	eventually(t, func() bool { return exists(c, key(ns, "prod-workers"), &md) }, "the MachineDeployment is created")
	var cl clusterv1.Cluster
	var nc v1alpha1.NaruCluster
	var cp v1alpha1.NaruControlPlane
	var cpt, wt v1alpha1.NaruMachineTemplate
	var cfg v1alpha1.NaruConfigTemplate
	require.NoError(t, c.Get(ctx, key(ns, "prod"), &cl))
	require.NoError(t, c.Get(ctx, key(ns, "prod"), &nc))
	require.NoError(t, c.Get(ctx, key(ns, "prod"), &cp))
	require.NoError(t, c.Get(ctx, key(ns, "prod-control-plane"), &cpt))
	require.NoError(t, c.Get(ctx, key(ns, "prod-workers"), &wt))
	require.NoError(t, c.Get(ctx, key(ns, "prod-workers"), &cfg))
	require.NoError(t, c.Get(ctx, key(ns, "prod"), d))

	for _, o := range []client.Object{&cl, &nc, &cp, &cpt, &wt, &cfg, &md} {
		requireControlledBy(t, d, o)
	}

	require.Equal(t, "NaruCluster", cl.Spec.InfrastructureRef.Kind)
	require.Equal(t, "prod", cl.Spec.InfrastructureRef.Name)
	require.Equal(t, "naru.garam.sh", cl.Spec.InfrastructureRef.APIGroup)
	require.Equal(t, "NaruControlPlane", cl.Spec.ControlPlaneRef.Kind)
	require.Equal(t, "prod", cl.Spec.ControlPlaneRef.Name)

	require.Equal(t, "eu", nc.Spec.Region)
	require.Equal(t, map[string]string{"tier": "gold"}, nc.Spec.HostSelector.MatchLabels)

	require.EqualValues(t, 3, *cp.Spec.Replicas)
	require.Equal(t, "v1.31.0", cp.Spec.Version)
	require.Equal(t, "NaruMachineTemplate", cp.Spec.MachineTemplate.Spec.InfrastructureRef.Kind)
	require.Equal(t, "prod-control-plane", cp.Spec.MachineTemplate.Spec.InfrastructureRef.Name)

	for _, tpl := range []*v1alpha1.NaruMachineTemplate{&cpt, &wt} {
		require.EqualValues(t, 2, tpl.Spec.Template.Spec.CPU)
		require.Zero(t, tpl.Spec.Template.Spec.Memory.Cmp(resource.MustParse("2Gi")))
	}

	require.Equal(t, "prod", md.Spec.ClusterName)
	require.EqualValues(t, 2, *md.Spec.Replicas)
	require.Equal(t, "v1.31.0", md.Spec.Template.Spec.Version)
	require.Equal(t, "NaruConfigTemplate", md.Spec.Template.Spec.Bootstrap.ConfigRef.Kind)
	require.Equal(t, "prod-workers", md.Spec.Template.Spec.Bootstrap.ConfigRef.Name)
	require.Equal(t, "NaruMachineTemplate", md.Spec.Template.Spec.InfrastructureRef.Kind)
	require.Equal(t, "prod-workers", md.Spec.Template.Spec.InfrastructureRef.Name)
	require.Equal(t, md.Spec.Selector.MatchLabels, md.Spec.Template.Labels, "the selector matches the template")

	eventually(t, func() bool {
		return exists(c, key(ns, "prod"), d) && meta.IsStatusConditionTrue(d.Status.Conditions, v1alpha1.DeclarationReconciled)
	}, "the declaration reports Reconciled")
	require.Equal(t, d.Generation, d.Status.ObservedGeneration)
}

func TestDeclaration_WorkersReplicasChangeUpdatesTheMachineDeployment(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	d := declare(ns, "scale")
	require.NoError(t, c.Create(ctx, d))
	var md clusterv1.MachineDeployment
	eventually(t, func() bool { return exists(c, key(ns, "scale-workers"), &md) && *md.Spec.Replicas == 2 }, "two workers")

	patch := client.MergeFrom(d.DeepCopy())
	d.Spec.Workers.Replicas = 5
	require.NoError(t, c.Patch(ctx, d, patch))

	eventually(t, func() bool { return exists(c, key(ns, "scale-workers"), &md) && *md.Spec.Replicas == 5 }, "five workers")
	var cp v1alpha1.NaruControlPlane
	require.NoError(t, c.Get(ctx, key(ns, "scale"), &cp))
	require.EqualValues(t, 3, *cp.Spec.Replicas, "the control plane is as declared")
}

func TestDeclaration_VersionChangeUpdatesTheControlPlaneAndTheWorkers(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	d := declare(ns, "upgrade")
	require.NoError(t, c.Create(ctx, d))
	var md clusterv1.MachineDeployment
	eventually(t, func() bool { return exists(c, key(ns, "upgrade-workers"), &md) }, "the objects exist")

	patch := client.MergeFrom(d.DeepCopy())
	d.Spec.Version = "v1.32.1"
	require.NoError(t, c.Patch(ctx, d, patch))

	var cp v1alpha1.NaruControlPlane
	eventually(t, func() bool {
		return exists(c, key(ns, "upgrade-workers"), &md) && exists(c, key(ns, "upgrade"), &cp) &&
			md.Spec.Template.Spec.Version == "v1.32.1" && cp.Spec.Version == "v1.32.1"
	}, "both follow the new version")
}

func TestDeclaration_DeletingItDeletesTheCluster(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	d := declare(ns, "gone")
	require.NoError(t, c.Create(ctx, d))
	var cl clusterv1.Cluster
	eventually(t, func() bool {
		return exists(c, key(ns, "gone"), &cl) && exists(c, key(ns, "gone"), d) && len(d.Finalizers) == 1
	}, "the Cluster exists and the declaration holds its finalizer")

	require.NoError(t, c.Delete(ctx, d))

	eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key(ns, "gone"), &cl)) }, "the Cluster is deleted")
	eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key(ns, "gone"), d)) }, "the declaration is released")
}

func TestDeclaration_IsReleasedOnlyWhenTheClusterIsGone(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	d := declare(ns, "slow")
	require.NoError(t, c.Create(ctx, d))
	var cl clusterv1.Cluster
	eventually(t, func() bool {
		return exists(c, key(ns, "slow"), &cl) && exists(c, key(ns, "slow"), d) && len(d.Finalizers) == 1
	}, "the Cluster exists")

	// Cluster API's own controllers hold the Cluster while they take the Machines down.
	patch := client.MergeFrom(cl.DeepCopy())
	cl.Finalizers = append(cl.Finalizers, "test.naru.garam.sh/hold")
	require.NoError(t, c.Patch(ctx, &cl, patch))

	require.NoError(t, c.Delete(ctx, d))
	eventually(t, func() bool { return exists(c, key(ns, "slow"), &cl) && cl.DeletionTimestamp != nil }, "the Cluster is being deleted")
	require.Never(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key(ns, "slow"), d)) },
		time.Second, 50*time.Millisecond, "the declaration is released while the Cluster is still there")

	patch = client.MergeFrom(cl.DeepCopy())
	cl.Finalizers = nil
	require.NoError(t, c.Patch(ctx, &cl, patch))
	eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key(ns, "slow"), d)) }, "the declaration is released once the Cluster is gone")
}

func TestDeclaration_DoesNotTakeAnObjectItDoesNotControl(t *testing.T) {
	c, ns := run(t)
	ctx := context.Background()
	foreign := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "theirs"},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{Kind: "Other", Name: "other", APIGroup: "example.com"},
		},
	}
	require.NoError(t, c.Create(ctx, foreign))
	d := declare(ns, "theirs")
	require.NoError(t, c.Create(ctx, d))

	eventually(t, func() bool {
		return exists(c, key(ns, "theirs"), d) &&
			meta.IsStatusConditionPresentAndEqual(d.Status.Conditions, v1alpha1.DeclarationReconciled, metav1.ConditionFalse)
	}, "the declaration reports the failure")
	require.Equal(t, v1alpha1.ReasonApplyFailed, meta.FindStatusCondition(d.Status.Conditions, v1alpha1.DeclarationReconciled).Reason)
	require.NoError(t, c.Get(ctx, key(ns, "theirs"), foreign))
	require.Empty(t, foreign.OwnerReferences, "the Cluster is not adopted")

	require.NoError(t, c.Delete(ctx, d))
	eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key(ns, "theirs"), d)) }, "the declaration is released")
	require.NoError(t, c.Get(ctx, key(ns, "theirs"), foreign), "the Cluster is not deleted")
	require.Nil(t, foreign.DeletionTimestamp)
}
