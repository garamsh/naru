package command_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReset_PostsTheScopeAndTheConfirmationToTheHost(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	var got map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/reset", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"scope": got["scope"]})
	})
	addr := hostServer(t, dep, mux)
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "provisioner", "--endpoint", addr, "--out", cfg)

	out, err := run(t, "reset", "--config", cfg, "--scope", "data", "--confirm", "host-1")

	require.NoError(t, err, out)
	assert.Equal(t, map[string]string{"scope": "data", "confirm": "host-1"}, got)
	assert.Contains(t, out, "scope data")

	_, err = run(t, "reset", "--config", cfg, "--scope", "data")
	assert.ErrorContains(t, err, "--confirm is required")
	_, err = run(t, "reset", "--config", cfg, "--confirm", "host-1")
	assert.ErrorContains(t, err, "--scope is required")
}

func TestReset_ARevokedPairRefusalSaysToRotateBeforeAReset(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/reset", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "the pair is revoked", "code": "revoked-pair"})
	})
	addr := hostServer(t, dep, mux)
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "provisioner", "--endpoint", addr, "--out", cfg)

	out, err := run(t, "reset", "--config", cfg, "--scope", "all", "--confirm", "host-1")

	require.Error(t, err)
	assert.Contains(t, err.Error()+out, "rotate the pair")
	assert.Contains(t, err.Error()+out, "the pair is revoked")
}
