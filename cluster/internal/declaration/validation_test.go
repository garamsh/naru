package declaration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
	"github.com/garamsh/naru/cluster/internal/testenv"
)

// The API server's validation is what refuses a declaration, so these tests need no controller.
// Each refusal is paired with the nearest declaration it accepts.

func TestValidation_ControlPlaneReplicas(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	for _, tc := range []struct {
		replicas int32
		refused  bool
	}{{1, false}, {3, false}, {2, true}, {0, true}, {5, true}} {
		d := declare(ns, "cp")
		d.Name = fmt.Sprintf("cp-%d", tc.replicas)
		d.Spec.ControlPlane.Replicas = tc.replicas
		err := c.Create(context.Background(), d)
		if tc.refused {
			require.True(t, apierrors.IsInvalid(err), "replicas %d: %v", tc.replicas, err)
			require.Contains(t, err.Error(), "spec.controlPlane.replicas")
		} else {
			require.NoError(t, err, "replicas %d", tc.replicas)
		}
	}
}

func TestValidation_Region(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	for _, tc := range []struct {
		name, region string
		refused      bool
	}{
		{"ok", "eu", false},
		{"dashed", "ap-south-1", false},
		{"empty", "", true},
		{"upper", "EU", true},
		{"toolong", strings.Repeat("a", 64), true},
	} {
		d := declare(ns, tc.name)
		d.Spec.Region = tc.region
		err := c.Create(context.Background(), d)
		if tc.refused {
			require.True(t, apierrors.IsInvalid(err), "region %q: %v", tc.region, err)
			require.Contains(t, err.Error(), "spec.region")
		} else {
			require.NoError(t, err, "region %q", tc.region)
		}
	}
}

func TestValidation_NameLength(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	ok := declare(ns, strings.Repeat("a", 49))
	require.NoError(t, c.Create(context.Background(), ok))
	long := declare(ns, strings.Repeat("a", 50))
	err := c.Create(context.Background(), long)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.Contains(t, err.Error(), "at most 49 characters")
}

func TestValidation_Version(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	good := declare(ns, "good")
	require.NoError(t, c.Create(context.Background(), good))
	bad := declare(ns, "bad")
	bad.Spec.Version = "1.31"
	err := c.Create(context.Background(), bad)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.Contains(t, err.Error(), "spec.version")
}

func TestValidation_RegionAndNodeCannotChange(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	ctx := context.Background()
	d := declare(ns, "fixed")
	require.NoError(t, c.Create(ctx, d))

	// The control: what may change, changes.
	patch := client.MergeFrom(d.DeepCopy())
	d.Spec.Workers.Replicas = 7
	d.Spec.HostSelector = metav1.LabelSelector{MatchLabels: map[string]string{"tier": "silver"}}
	require.NoError(t, c.Patch(ctx, d, patch))

	patch = client.MergeFrom(d.DeepCopy())
	d.Spec.Region = "us"
	err := c.Patch(ctx, d, patch)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.Contains(t, err.Error(), "region cannot change")

	require.NoError(t, c.Get(ctx, key(ns, "fixed"), d))
	patch = client.MergeFrom(d.DeepCopy())
	d.Spec.Node.Memory = resource.MustParse("4Gi")
	err = c.Patch(ctx, d, patch)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.Contains(t, err.Error(), "node cannot change")
}

func TestValidation_MachineTemplateCannotChange(t *testing.T) {
	c := testenv.Client(t)
	ns := testenv.Namespace(t, c)
	ctx := context.Background()
	tpl := &v1alpha1.NaruMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "size"},
		Spec: v1alpha1.NaruMachineTemplateSpec{Template: v1alpha1.NaruMachineTemplateResource{
			Spec: v1alpha1.NaruMachineSpec{CPU: 2, Memory: resource.MustParse("2Gi")},
		}},
	}
	require.NoError(t, c.Create(ctx, tpl))

	// The control: a write that leaves the template alone is accepted.
	patch := client.MergeFrom(tpl.DeepCopy())
	tpl.Labels = map[string]string{"a": "b"}
	require.NoError(t, c.Patch(ctx, tpl, patch))

	patch = client.MergeFrom(tpl.DeepCopy())
	tpl.Spec.Template.Spec.CPU = 4
	err := c.Patch(ctx, tpl, patch)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.Contains(t, err.Error(), "template cannot change")
}
