package node

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// The cloud-controller add-on's manifest is the in-cluster half of the credential boundary (ADR 0085
// item 7): what the controller's ServiceAccount may do in the cluster, and how its Pod holds the
// exposer pair.
const ccmManifest = "../../../../image/pkgs/ccm/ccm.yaml.in"

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
	Spec struct {
		Replicas int `yaml:"replicas"`
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				HostNetwork        bool   `yaml:"hostNetwork"`
				SecurityContext    struct {
					FSGroup *int64 `yaml:"fsGroup"`
				} `yaml:"securityContext"`
				Containers []struct {
					Image           string   `yaml:"image"`
					Args            []string `yaml:"args"`
					SecurityContext struct {
						Privileged               bool  `yaml:"privileged"`
						AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
						ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
						RunAsNonRoot             bool  `yaml:"runAsNonRoot"`
						Capabilities             struct {
							Add  []string `yaml:"add"`
							Drop []string `yaml:"drop"`
						} `yaml:"capabilities"`
					} `yaml:"securityContext"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret *struct {
						SecretName  string `yaml:"secretName"`
						DefaultMode int    `yaml:"defaultMode"`
					} `yaml:"secret"`
					HostPath *struct{} `yaml:"hostPath"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func ccmObjects(t *testing.T) map[string]object {
	t.Helper()
	b, err := os.ReadFile(ccmManifest)
	require.NoError(t, err)
	require.Contains(t, string(b), "@CCM_IMAGE@", "the build names the image by digest")
	b = bytes.ReplaceAll(b, []byte("@CCM_IMAGE@"), []byte("naru.local/ccm@sha256:test"))
	dec := yaml.NewDecoder(bytes.NewReader(b))
	out := map[string]object{}
	for {
		var o object
		if err := dec.Decode(&o); err == io.EOF {
			break
		} else {
			require.NoError(t, err)
		}
		out[o.Kind] = o
	}
	return out
}

func TestCCMManifest_TheServiceAccountMayNotReadSecretsOrTouchPodsAndHoldsNoWildcard(t *testing.T) {
	role := ccmObjects(t)["ClusterRole"]
	require.NotEmpty(t, role.Rules)
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			assert.NotEqual(t, "*", res)
			assert.NotContains(t, []string{"secrets", "pods", "configmaps", "serviceaccounts", "serviceaccounts/token", "nodes/proxy", "roles", "clusterroles", "rolebindings", "clusterrolebindings"}, res,
				"the controller has no business with %s: the credential it holds must not reach the cluster's secrets, nor mint a token for a ServiceAccount that does", res)
			assert.False(t, strings.HasSuffix(res, "/exec") || strings.HasSuffix(res, "/attach") || strings.HasSuffix(res, "/portforward"), "%s runs a command in a Pod", res)
		}
		assert.NotContains(t, r.Verbs, "*")
		assert.NotContains(t, r.APIGroups, "*")
	}
	nodes := slices.IndexFunc(role.Rules, func(r struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	}) bool {
		return slices.Contains(r.Resources, "nodes")
	})
	require.GreaterOrEqual(t, nodes, 0, "control: the rule the node controller needs is there")
}

func TestCCMManifest_ThePodHoldsTheExposerPairOnlyAsAReadOnlySecretAndRunsUnprivileged(t *testing.T) {
	d := ccmObjects(t)["Deployment"]
	assert.Equal(t, 2, d.Spec.Replicas)
	spec := d.Spec.Template.Spec
	assert.Equal(t, "cloud-controller-manager", spec.ServiceAccountName)
	require.Len(t, spec.Containers, 1)
	c := spec.Containers[0]
	assert.Equal(t, "naru.local/ccm@sha256:test", c.Image)
	assert.Contains(t, c.Args, "--cloud-provider=naru")

	sc := c.SecurityContext
	assert.False(t, sc.Privileged)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	assert.True(t, sc.ReadOnlyRootFilesystem)
	assert.True(t, sc.RunAsNonRoot)
	assert.Equal(t, []string{"ALL"}, sc.Capabilities.Drop)
	assert.Empty(t, sc.Capabilities.Add)

	require.Len(t, spec.Volumes, 1, "the Secret is the only volume")
	require.NotNil(t, spec.Volumes[0].Secret)
	assert.Equal(t, "naru-ccm", spec.Volumes[0].Secret.SecretName, "the operator makes it; nothing in the add-on does")
	assert.Nil(t, spec.Volumes[0].HostPath)
	// The Secret's files are root:<fsGroup> 0440 and the container is not root, so without the group it cannot read its credential.
	require.NotNil(t, spec.SecurityContext.FSGroup, "the group that makes the Secret readable to the container's user")
	assert.EqualValues(t, 65532, *spec.SecurityContext.FSGroup)
	assert.Equal(t, 0o440, spec.Volumes[0].Secret.DefaultMode)
	require.Len(t, c.VolumeMounts, 1)
	assert.True(t, c.VolumeMounts[0].ReadOnly)
	assert.True(t, strings.HasPrefix(strings.Join(c.Args, " "), "--cloud-provider"), "control: the args are read")
}

func TestCCMManifest_EveryObjectIsLabelledSoTheAddonsServicePrunesOnlyWhatItApplied(t *testing.T) {
	for kind, o := range ccmObjects(t) {
		assert.Equal(t, "naru-addons", o.Metadata.Labels["app.kubernetes.io/managed-by"], kind)
	}
}
