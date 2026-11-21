package volumes_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/node/internal/volumes"
)

func TestLoadConfig_ReadsTheFileAndRefusesOneWithoutAStateDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"stateDir":"/s","storageDir":"/d","blockSerials":["a"]}`), 0o600))
	c, err := volumes.LoadConfig(p)
	require.NoError(t, err)
	assert.Equal(t, volumes.Config{StateDir: "/s", StorageDir: "/d", BlockSerials: []string{"a"}}, c)
	require.NoError(t, os.WriteFile(p, []byte(`{}`), 0o600))
	_, err = volumes.LoadConfig(p)
	assert.Error(t, err)
}
