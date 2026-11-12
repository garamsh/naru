package clusterfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/clusterfile"
)

func TestWrite_ARoundTripKeepsEveryFieldAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	want := clusterfile.File{
		Version:   clusterfile.Version,
		APIServer: clusterfile.APIServer{Address: "127.0.0.1:6443"},
		PKI:       clusterfile.PKI{CA: "/p/ca.pem", ClientCert: "/p/client.pem", ClientKey: "/p/client.key"},
		Nodes: []clusterfile.Node{
			{Name: "node-1", PID: 11, SerialLog: "/n1/console.log", ControlPlane: true},
			{Name: "node-2", PID: 12, SerialLog: "/n2/console.log"},
		},
		Images: []clusterfile.Image{{Reference: "127.0.0.1:5000/test/entry@sha256:abc"}},
	}

	require.NoError(t, clusterfile.Write(dir, want))
	got, err := clusterfile.Read(filepath.Join(dir, clusterfile.Name))

	require.NoError(t, err)
	assert.Equal(t, want, got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only cluster.json is left")
}

func TestRead_RefusesAVersionItDoesNotKnowAndAcceptsAFieldItDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	newer := filepath.Join(dir, "newer.json")
	require.NoError(t, os.WriteFile(newer, []byte(`{"version": 2}`), 0o644))
	added := filepath.Join(dir, "added.json")
	require.NoError(t, os.WriteFile(added, []byte(`{"version": 1, "somethingNew": true}`), 0o644))

	_, err := clusterfile.Read(newer)
	assert.ErrorContains(t, err, "version 2")
	_, err = clusterfile.Read(added)
	assert.NoError(t, err, "an added optional field does not change the version")
}
