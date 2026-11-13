package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain_GetPrintsTheUserAndPasswordOfTheScript(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "credstub")
	build := exec.Command("go", "build", "-o", bin, ".") //nolint:gosec,noctx // fixed arguments
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	script := filepath.Join(t.TempDir(), "docker-credential-x")
	require.NoError(t, os.WriteFile(script, []byte("#!"+bin+"\nuser:pa:ss\n"), 0o600))

	cmd := exec.Command(bin, script, "get") //nolint:gosec,noctx // the program just built
	cmd.Stdin = strings.NewReader("127.0.0.1:5000\n")
	got, err := cmd.Output()

	require.NoError(t, err)
	assert.JSONEq(t, `{"ServerURL":"127.0.0.1:5000","Username":"user","Secret":"pa:ss"}`, string(got))
	assert.Error(t, exec.Command(bin, script, "list").Run(), "only get is answered") //nolint:gosec,noctx // the program just built
}
