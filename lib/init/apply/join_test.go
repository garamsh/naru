package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

const joinDoc = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
spec:
  join:
    orch:
      url: https://orch.example.test:7500
      caSHA256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    enrollKey: naru-ek-fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`

// rigRunning is a rig whose running configuration is doc.
func rigRunning(t *testing.T, doc string) *rig {
	t.Helper()
	r := &rig{}
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	r.engine = apply.New(apply.Deps{Store: r, Services: r, Files: r, Time: r, Routes: r, Registries: r, API: r}, store.Stored{Revision: 0, BaseSha256: "base", Config: cfg})
	return r
}

func TestApply_AJoinedHostTakesAConfigurationThatNamesItAndItsDataDisk(t *testing.T) {
	var full map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(baseDoc(t)), &full))

	r := rigRunning(t, joinDoc)
	res, err := r.engine.Apply(apply.Request{Config: full, Mode: apply.Reboot})

	require.NoError(t, err)
	assert.True(t, res.Reboot)
	require.Len(t, r.saved, 1)
	assert.Equal(t, "node-1", r.saved[0].Config.Metadata.Name)
	assert.Equal(t, "base", r.saved[0].BaseSha256, "the stored revision keeps the base of the join form")

	// The control: a host that did not join may not be renamed.
	r = rigRunning(t, baseDoc(t))
	renamed := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(baseDoc(t)), &renamed))
	renamed["metadata"] = map[string]any{"name": "other"}
	_, err = r.engine.Apply(apply.Request{Config: renamed, Mode: apply.Reboot})
	require.Error(t, err)
	assert.Empty(t, r.saved)
}
