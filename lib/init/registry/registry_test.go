package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/registry"
)

func specWith() config.Spec {
	return config.Spec{
		Registries: []config.Registry{
			{Host: "r.example:5000", CA: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", Credential: &config.Credential{Username: "u", Password: "p"}},
			{Host: "ecr.example", Credential: &config.Credential{Helper: "ecr-login"}},
			{Host: "open.example"},
		},
		AWS: &config.AWS{Region: "ap-northeast-2", TrustAnchorArn: "arn:ta", ProfileArn: "arn:pr", RoleArn: "arn:ro", Certificate: "/c.pem", Key: "/k.pem"},
	}
}

func TestWrite_WritesCredentialsAndCA(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, registry.Writer{Dir: dir}.Write(specWith()))

	raw, err := os.ReadFile(filepath.Join(dir, "registry/credentials.json"))
	require.NoError(t, err)
	var creds map[string]map[string]string
	require.NoError(t, json.Unmarshal(raw, &creds))
	assert.Equal(t, map[string]map[string]string{"r.example:5000": {"username": "u", "password": "p"}}, creds, "a helper or no credential has no entry")
	st, _ := os.Stat(filepath.Join(dir, "registry/credentials.json"))
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	ca, err := os.ReadFile(filepath.Join(dir, "registry/ca.pem"))
	require.NoError(t, err)
	assert.Contains(t, string(ca), "BEGIN CERTIFICATE")
	assert.NoDirExists(t, filepath.Join(dir, "aws"), "the credential comes from the aws-credentials service, not from a profile file")
}

func TestWrite_MakesTheHomeDirectoryPrivateWhateverModeItHad(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "registry/home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.Chmod(home, 0o755))

	require.NoError(t, registry.Writer{Dir: dir}.Write(config.Spec{}))

	st, err := os.Stat(home)
	require.NoError(t, err)
	assert.True(t, st.IsDir())
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
}

func TestWrite_WritesAnEmptyCredentialsFileAndRemovesWhatIsNoLongerConfigured(t *testing.T) {
	dir := t.TempDir()
	w := registry.Writer{Dir: dir}
	require.NoError(t, w.Write(specWith()))

	require.NoError(t, w.Write(config.Spec{}))

	raw, err := os.ReadFile(filepath.Join(dir, "registry/credentials.json"))
	require.NoError(t, err, "mvmd refuses a configuration that names a file that is not there")
	assert.JSONEq(t, "{}", string(raw))
	assert.NoFileExists(t, filepath.Join(dir, "registry/ca.pem"))
	assert.DirExists(t, filepath.Join(dir, "registry/home"), "a credential helper's log has a place whatever is configured")
}
