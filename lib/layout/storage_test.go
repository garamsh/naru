package layout_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/wire"
)

// emittedMvmd is what a Node's user-data carries of mvmd's configuration and of spec.storage.
func emittedMvmd(t *testing.T, n layout.Node) (mvmd map[string]any, storage []struct{ Name, MountPoint string }) {
	t.Helper()
	data, err := n.UserData()
	require.NoError(t, err)
	var c struct {
		Spec struct {
			Files []struct {
				Path     string `yaml:"path"`
				Content  string `yaml:"content"`
				Template string `yaml:"template"`
			} `yaml:"files"`
			Storage []struct {
				Name       string `yaml:"name"`
				MountPoint string `yaml:"mountPoint"`
			} `yaml:"storage"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &c))
	for _, f := range c.Spec.Files {
		if f.Path == layout.MvmdConfig {
			mvmd = render(t, config.File{Path: f.Path, Content: f.Content, Template: f.Template},
				fmt.Sprintf(guestOf, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)))
		}
	}
	require.NotNil(t, mvmd, "the user-data writes mvmd's configuration")
	for _, s := range c.Spec.Storage {
		storage = append(storage, struct{ Name, MountPoint string }{s.Name, s.MountPoint})
	}
	return mvmd, storage
}

func TestStorage_KeepsMvmdsStateOnTheDataDiskAndPutsItsRunDirectoriesOnTheFirstStorageDisk(t *testing.T) {
	n := node(t, func(n *layout.Node) {
		n.Storage = []layout.StorageDisk{{Name: "vmdisks", Label: "vmdisks"}, {Name: "scratch", Label: "scratch"}}
	})

	mvmd, storage := emittedMvmd(t, n)

	assert.Equal(t, wire.DataMount+"/mvmd", mvmd["state_dir"], "state_dir stays on the data disk, whose file system has fs-verity")
	assert.Equal(t, wire.MvmdStorageMount+"/machines", mvmd["supervision"].(map[string]any)["dir"])
	require.Len(t, storage, 2)
	assert.Equal(t, wire.MvmdStorageMount, storage[0].MountPoint, "the first disk is mounted once, outside state_dir")
	assert.Empty(t, storage[1].MountPoint, "the others take init's default")
}

func TestStorage_WithoutItMvmdsSupervisionDirectoryIsLeftToItsDefaultUnderStateDir(t *testing.T) {
	mvmd, storage := emittedMvmd(t, node(t, func(*layout.Node) {}))

	assert.Equal(t, wire.DataMount+"/mvmd", mvmd["state_dir"])
	assert.NotContains(t, mvmd["supervision"], "dir")
	assert.Empty(t, storage)
}
