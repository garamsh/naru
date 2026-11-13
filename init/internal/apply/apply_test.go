package apply_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/apply"
	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/pkitest"
	"github.com/garamsh/naru/init/internal/store"
)

// rig is an engine over fakes that write every call to one journal, so a test
// sees the order across them.
type rig struct {
	engine  *apply.Engine
	mu      sync.Mutex
	journal []string
	saved   []store.Stored
	specs   []config.PKI // the spec each Reissue was given
	failOn  string       // a journal entry that fails
}

func (r *rig) note(entry string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.journal = append(r.journal, entry)
	if entry == r.failOn {
		return errors.New("boom")
	}
	return nil
}

func (r *rig) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.journal...)
}

func (r *rig) Save(st store.Stored) error {
	r.saved = append(r.saved, st)
	return r.note("store")
}
func (r *rig) Start(s config.Service) error    { return r.note("start " + s.Name) }
func (r *rig) Stop(name string)                { _ = r.note("stop " + name) }
func (r *rig) Restart(name string)             { _ = r.note("restart " + name) }
func (r *rig) ReplaceFile(f config.File) error { return r.note("file " + f.Path) }
func (r *rig) Reissue(spec config.PKI, names []string) error {
	r.specs = append(r.specs, spec)
	return r.note(fmt.Sprintf("issue %v", names))
}
func (r *rig) WriteRegistries(config.Spec) error { return r.note("write-registries") }
func (r *rig) Reload(config.API) error           { return r.note("reload") }
func (r *rig) AddRoute(rt config.Route) error {
	return r.note(fmt.Sprintf("add-route %s via %s", rt.To, rt.Via))
}
func (r *rig) DeleteRoute(rt config.Route) error {
	return r.note(fmt.Sprintf("delete-route %s via %s", rt.To, rt.Via))
}
func (r *rig) ConfigureTime(servers []string) {
	_ = r.note(fmt.Sprintf("time %v", servers))
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{}
	cfg, err := config.Parse([]byte(baseDoc(t)))
	require.NoError(t, err)
	r.engine = apply.New(apply.Deps{Store: r, Services: r, Files: r, Time: r, Routes: r, Certificates: r, Registries: r, API: r}, store.Stored{Revision: 0, BaseSha256: "base", Config: cfg})
	return r
}

func baseDoc(t *testing.T) string {
	t.Helper()
	ca := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	return `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: a
      binary: /bin/a
      args: ["-x", "1"]
      env:
        K: v
    - name: b
      binary: /bin/b
    - name: e
      binary: /bin/e
    - name: cp
      binary: /bin/cp
      controlPlane: true
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  network:
    forwarding: [ipv4]
  files:
    - path: /run/f1
      content: one
      restart: [a]
    - path: /run/f2
      mode: "0600"
      content: two
  pki:
    authorities:
      - name: ca
        certificatePath: /run/ca.pem
        certificate: |
` + pkitest.Indent(ca.Cert, 10) + `        key: |
` + pkitest.Indent(ca.Key, 10) + `    certificates:
      - name: c1
        authority: ca
        certificatePath: /run/c1.pem
        keyPath: /run/c1.key
        commonName: c1
        usage: client
        restart: [b]
  api:
    clientCA: |
` + pkitest.Indent(ca.Cert, 6) + `    certificate: |
` + pkitest.Indent(ca.Cert, 6) + `    key: |
` + pkitest.Indent(ca.Key, 6)
}

type obj = map[string]any

func patch(spec obj) obj { return obj{"spec": spec} }

func (r *rig) staged(t *testing.T, p obj) config.NodeConfig {
	t.Helper()
	rev, _ := r.engine.Config()
	_, err := r.engine.Apply(apply.Request{Revision: rev, Patch: p, Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg := r.engine.Config()
	return cfg
}

func TestMerge(t *testing.T) {
	t.Run("a patch merges into nested objects and replaces other lists whole", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{"services": []any{obj{"name": "a", "args": []any{"-y"}, "env": obj{"L": "w"}}}}))

		assert.Equal(t, []string{"-y"}, cfg.Spec.Services[0].Args)
		assert.Equal(t, map[string]string{"K": "v", "L": "w"}, cfg.Spec.Services[0].Env)
		assert.Equal(t, "/bin/a", cfg.Spec.Services[0].Binary, "a field the patch leaves out stays")
	})
	t.Run("null removes a key", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{"services": []any{obj{"name": "a", "env": obj{"K": nil}}}}))

		assert.Empty(t, cfg.Spec.Services[0].Env)
	})
	t.Run("an element that is not in the patch is unchanged and keeps its place", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{"services": []any{obj{"name": "b", "args": []any{"q"}}}}))

		require.Len(t, cfg.Spec.Services, 4)
		assert.Equal(t, []string{"a", "b", "e", "cp"}, []string{cfg.Spec.Services[0].Name, cfg.Spec.Services[1].Name, cfg.Spec.Services[2].Name, cfg.Spec.Services[3].Name})
		assert.Equal(t, []string{"-x", "1"}, cfg.Spec.Services[0].Args)
	})
	t.Run("an element with a new key is appended", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{"services": []any{obj{"name": "d", "binary": "/bin/d"}}}))

		require.Len(t, cfg.Spec.Services, 5)
		assert.Equal(t, "d", cfg.Spec.Services[4].Name)
	})
	t.Run("$patch delete removes the element", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{"services": []any{obj{"name": "e", "$patch": "delete"}}}))

		require.Len(t, cfg.Spec.Services, 3)
		assert.Equal(t, []string{"a", "b", "cp"}, []string{cfg.Spec.Services[0].Name, cfg.Spec.Services[1].Name, cfg.Spec.Services[2].Name})
	})
	t.Run("files merge by path and certificates by name", func(t *testing.T) {
		cfg := newRig(t).staged(t, patch(obj{
			"files": []any{obj{"path": "/run/f2", "content": "changed"}},
			"pki":   obj{"certificates": []any{obj{"name": "c1", "commonName": "renamed"}}},
		}))

		assert.Equal(t, "changed", cfg.Spec.Files[1].Content)
		assert.Equal(t, "0600", cfg.Spec.Files[1].Mode)
		assert.Equal(t, "renamed", cfg.Spec.PKI.Certificates[0].CommonName)
	})
	t.Run("a delete of a key that does not exist is refused naming the element", func(t *testing.T) {
		r := newRig(t)

		_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "ghost", "$patch": "delete"}}}), Mode: apply.Staged})

		var invalid *apply.InvalidError
		require.ErrorAs(t, err, &invalid)
		assert.Equal(t, []string{"spec.services[0]"}, invalid.Fields)
		assert.Empty(t, r.calls(), "nothing was stored")
	})
	t.Run("an element without its key is refused", func(t *testing.T) {
		_, err := newRig(t).engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"binary": "/bin/z"}}}), Mode: apply.Staged})

		var invalid *apply.InvalidError
		require.ErrorAs(t, err, &invalid)
		assert.Equal(t, []string{"spec.services[0].name"}, invalid.Fields)
	})
}

func TestApply_RefusesWithoutChangingTheNode(t *testing.T) {
	tests := []struct {
		name  string
		req   apply.Request
		check func(t *testing.T, err error)
	}{
		{"a stale revision", apply.Request{Revision: 4, Patch: obj{}, Mode: apply.NoReboot}, func(t *testing.T, err error) {
			var stale *apply.StaleError
			require.ErrorAs(t, err, &stale)
			assert.Equal(t, 0, stale.Stored)
			assert.Equal(t, 4, stale.Requested)
		}},
		{"no mode", apply.Request{Patch: obj{}}, invalidAt("mode")},
		{"neither patch nor config", apply.Request{Mode: apply.Staged}, invalidAt("patch", "config")},
		{"both patch and config", apply.Request{Patch: obj{}, Config: obj{}, Mode: apply.Staged}, invalidAt("patch", "config")},
		{"a result that does not validate", apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "binary": "relative"}}}), Mode: apply.NoReboot}, invalidAt("spec.services[0].binary")},
		{"an unknown field", apply.Request{Patch: patch(obj{"bogus": 1}), Mode: apply.Staged}, invalidAt("spec.bogus")},
		{"a never field in no-reboot", apply.Request{Patch: obj{"metadata": obj{"name": "other"}}, Mode: apply.NoReboot}, invalidAt("metadata.name")},
		{"a never field in staged", apply.Request{Patch: patch(obj{"dataDisk": obj{"mountPoint": "/mnt"}}), Mode: apply.Staged}, invalidAt("spec.dataDisk")},
		{"a reboot-only field in no-reboot", apply.Request{Patch: patch(obj{"controlPlane": true, "network": obj{"forwarding": []any{"ipv4", "ipv6"}}}), Mode: apply.NoReboot}, invalidAt("spec.controlPlane", "spec.network.forwarding")},
		{"a changed authority in no-reboot", apply.Request{Patch: patch(obj{"pki": obj{"authorities": []any{obj{"name": "ca", "certificatePath": "/run/other.pem"}}}}), Mode: apply.NoReboot}, invalidAt("spec.pki.authorities")},
		{"a new listen address in no-reboot", apply.Request{Patch: patch(obj{"api": obj{"listen": "127.0.0.1:50001"}}), Mode: apply.NoReboot}, invalidAt("spec.api.listen")},
		{"turning spec.api off in no-reboot", apply.Request{Patch: patch(obj{"api": nil}), Mode: apply.NoReboot}, invalidAt("spec.api")},
		{"REDACTED for a key", apply.Request{Patch: patch(obj{"api": obj{"key": "REDACTED"}}), Mode: apply.Staged}, invalidAt("spec.api.key")},
		{"REDACTED for a private file's content", apply.Request{Patch: patch(obj{"files": []any{obj{"path": "/run/f2", "content": "REDACTED"}}}), Mode: apply.Staged}, invalidAt("spec.files[1].content")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)

			res, err := r.engine.Apply(tt.req)

			require.Error(t, err)
			tt.check(t, err)
			assert.Zero(t, res)
			assert.Empty(t, r.calls(), "nothing stored, nothing live")
			rev, _ := r.engine.Config()
			assert.Equal(t, 0, rev)
		})
	}
}

func invalidAt(fields ...string) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		var invalid *apply.InvalidError
		require.ErrorAs(t, err, &invalid)
		assert.Equal(t, fields, invalid.Fields)
	}
}

func TestApply_REDACTEDOnAPublicFileIsContent(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"files": []any{obj{"path": "/run/f1", "content": "REDACTED"}}}), Mode: apply.Staged})

	require.NoError(t, err, "only the content of a file that is private stands for hidden material")
}

func TestApply_StagedStoresAndTakesNoLiveAction(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"controlPlane": true}), Mode: apply.Staged})

	require.NoError(t, err)
	assert.Equal(t, apply.Result{Revision: 1, Mode: apply.Staged, Actions: []apply.Action{{Action: "store", Target: "config"}}}, res)
	assert.Equal(t, []string{"store"}, r.calls())
	assert.True(t, r.saved[0].Config.Spec.ControlPlane)
	assert.Equal(t, "base", r.saved[0].BaseSha256, "the base digest stays")
}

func TestApply_AStagedRebootOnlyChangeMakesNoRebootRefuseUntilARebootRunsIt(t *testing.T) {
	r := newRig(t)
	r.staged(t, patch(obj{"controlPlane": true}))

	_, err := r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{"z"}}}}), Mode: apply.NoReboot})

	invalidAt("spec.controlPlane")(t, err)
}

func TestApply_NoRebootTouchesOnlyWhatChanged(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{
		obj{"name": "a", "args": []any{"-y"}},
		obj{"name": "d", "binary": "/bin/d"},
		obj{"name": "e", "$patch": "delete"},
	}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "stop e", "stop a", "start a", "start d"}, r.calls(),
		"the store comes first; service cp, which runs only on a control-plane Node, is not touched, and neither is any unchanged service")
	assert.Equal(t, []apply.Action{{"store", "config"}, {"stop", "e"}, {"restart", "a"}, {"start", "d"}}, res.Actions)
	assert.Equal(t, 1, res.Revision)
}

func TestApply_AServiceThisNodeDoesNotRunIsStoredAndNotStarted(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "cp", "args": []any{"y"}}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store"}, r.calls(), "a service this Node does not run is stored and not started")
	assert.Len(t, res.Actions, 1)
}

func TestApply_AFileThatChangedIsWrittenAndItsServicesRestartOnce(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"files": []any{
		obj{"path": "/run/f1", "content": "uno"},
		obj{"path": "/run/f2", "mode": "0640", "restart": []any{"a", "b"}},
		obj{"path": "/run/f3", "content": "three", "restart": []any{"b"}},
	}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "file /run/f1", "file /run/f2", "file /run/f3", "restart a", "restart b"}, r.calls())
}

func TestApply_ANewRestartListAloneRestartsNothing(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"files": []any{obj{"path": "/run/f1", "restart": []any{"b"}}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store"}, r.calls())
}

func TestApply_AServiceThatTheApplyRestartedIsNotRestartedAgainForAFile(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{
		"services": []any{obj{"name": "a", "args": []any{"-y"}}},
		"files":    []any{obj{"path": "/run/f1", "content": "uno"}},
	}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "file /run/f1", "stop a", "start a"}, r.calls(), "the file is written before the service starts again, and the service restarts once")
}

func TestApply_ACertificateThatChangedIsIssuedAgainAndItsServicesRestart(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"pki": obj{"certificates": []any{obj{"name": "c1", "commonName": "renamed"}}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "issue [c1]", "restart b"}, r.calls())
}

func TestApply_ADeletedCertificateIsHandedToTheIssuerSoItIsNotRenewed(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"pki": obj{"certificates": []any{obj{"name": "c1", "$patch": "delete"}}}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "issue []"}, r.calls(), "no certificate is signed and no service restarts")
	require.Len(t, r.specs, 1)
	assert.Empty(t, r.specs[0].Certificates, "the issuer is given a spec without the deleted certificate")
}

func TestApply_ANewAPIPairReloadsTheListener(t *testing.T) {
	r := newRig(t)
	other := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"api": obj{"certificate": other.Cert, "key": other.Key}}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "reload"}, r.calls())
}

func TestApply_ConfigReplacesTheConfiguration(t *testing.T) {
	r := newRig(t)
	_, cfg := r.engine.Config()
	cfg.Spec.Services = []config.Service{cfg.Spec.Services[0], cfg.Spec.Services[1], cfg.Spec.Services[3]}
	doc := toObj(t, cfg)

	res, err := r.engine.Apply(apply.Request{Config: doc, Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "stop e"}, r.calls())
	assert.Equal(t, 1, res.Revision)
}

func TestApply_DryRunListsTheActionsAndDoesNone(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{"-y"}}}}), Mode: apply.NoReboot, DryRun: true})

	require.NoError(t, err)
	assert.Equal(t, []apply.Action{{"store", "config"}, {"restart", "a"}}, res.Actions)
	assert.True(t, res.DryRun)
	assert.Equal(t, 0, res.Revision, "a dry run does not move the revision")
	assert.Empty(t, r.calls(), "no store write and no live action")
	rev, cfg := r.engine.Config()
	assert.Equal(t, 0, rev)
	assert.Equal(t, []string{"-x", "1"}, cfg.Spec.Services[0].Args)
}

func TestApply_ADryRunRefusesAsARealCallDoes(t *testing.T) {
	_, err := newRig(t).engine.Apply(apply.Request{Patch: patch(obj{"controlPlane": true}), Mode: apply.NoReboot, DryRun: true})

	invalidAt("spec.controlPlane")(t, err)
}

func TestApply_ASecondCallMustNameTheNewRevision(t *testing.T) {
	r := newRig(t)
	p := patch(obj{"services": []any{obj{"name": "a", "args": []any{"-y"}}}})
	_, err := r.engine.Apply(apply.Request{Patch: p, Mode: apply.NoReboot})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 0, Patch: p, Mode: apply.NoReboot})
	var stale *apply.StaleError
	require.ErrorAs(t, err, &stale)

	res, err := r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{"-z"}}}}), Mode: apply.NoReboot})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Revision)
}

func TestApply_AFailedStoreLeavesTheNodeUnchanged(t *testing.T) {
	r := newRig(t)
	r.failOn = "store"

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{"-y"}}}}), Mode: apply.NoReboot})

	var stored *apply.StoreError
	require.ErrorAs(t, err, &stored)
	assert.Equal(t, []string{"store"}, r.calls(), "no live action followed")
	rev, _ := r.engine.Config()
	assert.Equal(t, 0, rev)
}

func TestApply_AFailedLiveActionIsReportedAfterTheStoreAndNothingIsUndone(t *testing.T) {
	r := newRig(t)
	r.failOn = "start d"

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{
		obj{"name": "a", "args": []any{"-y"}},
		obj{"name": "d", "binary": "/bin/d"},
		obj{"name": "f", "binary": "/bin/f"},
	}}), Mode: apply.NoReboot})

	var failed *apply.FailedError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, 1, failed.Revision)
	assert.Equal(t, []apply.Action{{"store", "config"}, {"restart", "a"}}, failed.Done)
	assert.Equal(t, apply.Action{Action: "start", Target: "d"}, failed.Failed)
	assert.Equal(t, []string{"store", "stop a", "start a", "start d"}, r.calls(), "nothing after the failure ran, and nothing was undone")
	rev, cfg := r.engine.Config()
	assert.Equal(t, 1, rev, "the stored revision stays")
	assert.Len(t, cfg.Spec.Services, 6)
}

func TestApply_AppliesAreSerialised(t *testing.T) {
	r := newRig(t)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{fmt.Sprint(i)}}}}), Mode: apply.NoReboot})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var ok, stale int
	for err := range results {
		var s *apply.StaleError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &s):
			stale++
		default:
			t.Fatal(err)
		}
	}
	assert.Equal(t, 1, ok, "one of the calls that named revision 0 won")
	assert.Equal(t, n-1, stale)
	assert.Equal(t, []string{"store", "stop a", "start a"}, r.calls())
}

func registryPatch(host, password string) map[string]any {
	return obj{"registries": []any{obj{"host": host, "credential": obj{"username": "u", "password": password}}}}
}

func TestApply_RegistriesAreRewrittenLiveAndRestartNothing(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(registryPatch("r.example", "s3cret")), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.Equal(t, []string{"store", "write-registries"}, r.calls(), "the files are rewritten and no service restarts")
	assert.Equal(t, []apply.Action{{"store", "config"}, {"write-registries", "spec.registries"}}, res.Actions)
}

func TestApply_UnchangedRegistriesWriteNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.engine.Apply(apply.Request{Patch: patch(registryPatch("r.example", "s3cret")), Mode: apply.NoReboot})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"controlPlane": false}), Mode: apply.NoReboot})

	require.NoError(t, err)
	assert.NotContains(t, r.calls()[2:], "write-registries")
}

func TestApply_REDACTEDPasswordIsRefusedWhereNoneWasStored(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(registryPatch("r.example", "REDACTED")), Mode: apply.Staged})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, []string{"spec.registries[0].credential.password"}, invalid.Fields)
}

func TestApply_REDACTEDPasswordKeepsTheStoredOne(t *testing.T) {
	r := newRig(t)
	_, err := r.engine.Apply(apply.Request{Patch: patch(registryPatch("r.example", "s3cret")), Mode: apply.Staged})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"registries": []any{obj{"host": "r.example", "credential": obj{"username": "v"}}}}), Mode: apply.Staged})

	require.NoError(t, err)
	got := r.saved[len(r.saved)-1].Config.Spec.Registries[0].Credential
	assert.Equal(t, "v", got.Username)
	assert.Equal(t, "s3cret", got.Password, "a patch that omits the password keeps the stored one")
}
