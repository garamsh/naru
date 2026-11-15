package run

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
)

// exited is a service manager whose join service has exited with status 0.
type exited struct{ ch chan struct{} }

func newExited() exited {
	e := exited{make(chan struct{})}
	close(e.ch)
	return e
}

func (e exited) Exited(name string) <-chan struct{} {
	if name != joinService {
		return nil
	}
	return e.ch
}

// engineStub records what the handoff asks the apply engine, and answers with err.
type engineStub struct {
	revision int
	requests []apply.Request
	err      error
}

func (e *engineStub) Config() (int, config.NodeConfig) { return e.revision, config.NodeConfig{} }
func (e *engineStub) Apply(r apply.Request) (apply.Result, error) {
	e.requests = append(e.requests, r)
	if e.err != nil {
		return apply.Result{}, e.err
	}
	return apply.Result{Revision: e.revision + 1, Mode: r.Mode, Reboot: true}, nil
}

const joinedDoc = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: host-1
spec:
  controlPlane: false
`

// handoff runs handOff over a configuration file holding doc, and returns the stub, the file's
// path, what was logged and whether init rebooted.
func handoff(t *testing.T, doc string, stub *engineStub) (path, logs string, rebooted bool) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "config.yaml")
	if doc != "" {
		require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	handOff(t.Context(), newExited(), stub, path, func() { rebooted = true }, log)
	return path, out.String(), rebooted
}

func TestHandOff_AppliesTheConfigurationTheJoinServiceWroteAsTheWholeConfigurationInRebootMode(t *testing.T) {
	stub := &engineStub{revision: 0}

	path, logs, rebooted := handoff(t, joinedDoc, stub)

	require.Len(t, stub.requests, 1)
	req := stub.requests[0]
	assert.Equal(t, apply.Reboot, req.Mode)
	assert.Nil(t, req.Patch)
	assert.Equal(t, "host-1", req.Config["metadata"].(map[string]any)["name"])
	assert.Equal(t, 0, req.Revision, "against the revision that runs")
	assert.NoFileExists(t, path, "the file is removed once the store is written")
	assert.NoFileExists(t, path+".refused")
	assert.Contains(t, logs, "join configuration applied revision=1")
	assert.True(t, rebooted, "the host reboots into the configuration it was given")
}

func TestHandOff_RefusesAConfigurationThatTheEngineRefusesAndSetsItAside(t *testing.T) {
	stub := &engineStub{err: &apply.InvalidError{Fields: []string{"spec.services"}, Message: "no"}}

	path, logs, rebooted := handoff(t, joinedDoc, stub)

	assert.NoFileExists(t, path)
	assert.FileExists(t, path+".refused")
	assert.Contains(t, logs, "join configuration refused")
	assert.Contains(t, logs, "spec.services")
	assert.False(t, rebooted)
	assert.NotContains(t, logs, "join configuration applied")
}

func TestHandOff_RefusesAConfigurationThatCarriesASignerAnOrchOrAJoinSection(t *testing.T) {
	sections := map[string]string{
		"signer": "  signer:\n    listen: 127.0.0.1:1\n",
		"orch":   "  orch:\n    listen: 127.0.0.1:2\n",
		"join":   "  join:\n    enrollKey: x\n",
	}
	// The control is the first test: the same document without the section is applied.
	for name, section := range sections {
		t.Run(name, func(t *testing.T) {
			stub := &engineStub{}

			path, logs, rebooted := handoff(t, joinedDoc+section, stub)

			assert.Empty(t, stub.requests, "the engine is never asked")
			assert.FileExists(t, path+".refused")
			assert.Contains(t, logs, "join configuration refused")
			assert.Contains(t, logs, "spec."+name)
			assert.False(t, rebooted)
		})
	}
}

func TestHandOff_RefusesAFileThatIsNotYAML(t *testing.T) {
	stub := &engineStub{}

	path, logs, _ := handoff(t, "spec: [unterminated\n", stub)

	assert.Empty(t, stub.requests)
	assert.FileExists(t, path+".refused")
	assert.Contains(t, logs, "join configuration refused")
}

func TestHandOff_DoesNothingWhenTheJoinServiceWroteNoConfiguration(t *testing.T) {
	stub := &engineStub{}

	path, logs, rebooted := handoff(t, "", stub)

	assert.Empty(t, stub.requests)
	assert.NoFileExists(t, path+".refused")
	assert.NotContains(t, logs, "join configuration")
	assert.False(t, rebooted)
}

func TestHandOff_WaitsForTheJoinServiceToExit(t *testing.T) {
	stub := &engineStub{}
	ctx, cancel := context.WithCancel(t.Context())
	never := exits(neverExits{})
	done := make(chan struct{})

	go func() {
		handOff(ctx, never, stub, filepath.Join(t.TempDir(), "config.yaml"), func() {}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the handoff went on before the join service exited")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff did not stop when its context ended")
	}
	assert.Empty(t, stub.requests)
}

type neverExits struct{}

func (neverExits) Exited(string) <-chan struct{} { return make(chan struct{}) }
