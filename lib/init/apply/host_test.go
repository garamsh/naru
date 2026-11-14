package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

func hostRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{}
	cfg, err := config.Parse([]byte(baseDoc(t) + "\n  host:\n    region: eu-1\n    labels:\n      disk: ssd\n"))
	require.NoError(t, err)
	r.engine = apply.New(apply.Deps{Store: r, Services: r, Files: r, Time: r, Routes: r, Certificates: r, Registries: r, API: r}, store.Stored{Revision: 0, BaseSha256: "base", Config: cfg})
	return r
}

func TestApply_ALabelOfSpecHostChangesLiveByWritingOneFile(t *testing.T) {
	r := hostRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"host": obj{"labels": obj{"disk": "nvme"}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "file " + config.HostFile}, r.calls(), "no service is restarted and nothing needs a reboot")
	_, cfg := r.engine.Config()
	assert.Contains(t, cfg.Spec.Files[len(cfg.Spec.Files)-1].Content, "nvme")
	assert.Len(t, cfg.Spec.Files, 3, "the derived file is replaced in the stored configuration, not added to")
}

func TestApply_AnUnchangedSpecHostWritesNothing(t *testing.T) {
	r := hostRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"host": obj{"region": "eu-1"}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store"}, r.calls())
}
