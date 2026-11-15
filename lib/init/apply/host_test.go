package apply_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/wire"
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

func TestApply_AConfigurationTakenFromGETAndAppliedBackChangesNoSecret(t *testing.T) {
	r := &rig{}
	cfg, err := config.Parse([]byte(`apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: naru-hostd
      binary: /bin/naru-hostd
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  files:
    - path: /run/private
      mode: "0600"
      content: private-content
  host:
    region: eu-1
    orch:
      url: https://192.168.0.10:7500
      caSHA256: ` + strings.Repeat("ab", 32) + `
      enrollKey: ` + wire.EnrollKeyPrefix + strings.Repeat("cd", 32) + `
`))
	require.NoError(t, err)
	r.engine = apply.New(apply.Deps{Store: r, Services: r, Files: r, Time: r, Routes: r, Certificates: r, Registries: r, API: r}, store.Stored{Revision: 0, BaseSha256: "base", Config: cfg})
	_, held := r.engine.Config()
	data, err := yaml.Dump(apply.Redact(held))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Load(data, &doc))
	require.Equal(t, apply.Redacted, doc["spec"].(map[string]any)["host"].(map[string]any)["orch"].(map[string]any)["enrollKey"], "the GET form hides the enrollment key")

	_, err = r.engine.Apply(apply.Request{Revision: 0, Config: doc, Mode: apply.NoReboot})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Fields, "spec.host.orch.enrollKey", invalid.Error())
	assert.Empty(t, r.calls(), "nothing stored and no file written")
	_, after := r.engine.Config()
	assert.Equal(t, held, after, "no secret changed")
}

func TestApply_IsRefusedWhileTheStoreCannotBeReadAndNothingIsDone(t *testing.T) {
	r := &rig{}
	cfg, err := config.Parse([]byte(baseDoc(t)))
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, store.FileName), 0o700), "a read of store.yaml fails with an error other than not-exist")
	r.engine = apply.New(apply.Deps{Store: store.New(dir), Services: r, Files: r, Time: r, Routes: r, Certificates: r, Registries: r, API: r}, store.Stored{Revision: 0, BaseSha256: "base", Config: cfg})

	_, err = r.engine.Apply(apply.Request{Patch: patch(obj{"files": []any{obj{"path": "/run/f1", "content": "changed"}}}), Mode: apply.NoReboot})

	var storeErr *apply.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.ErrorIs(t, err, store.ErrUnreadable)
	assert.Empty(t, r.calls(), "no live action")
	rev, _ := r.engine.Config()
	assert.Equal(t, 0, rev)
	info, statErr := os.Stat(filepath.Join(dir, store.FileName))
	require.NoError(t, statErr)
	assert.True(t, info.IsDir(), "store.yaml is untouched")
}
