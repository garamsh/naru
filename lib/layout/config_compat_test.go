package layout_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/layout"
)

// What layout writes, lib/init reads: the writer and the reader are two modules with two YAML
// libraries (ADR 0070), and this is the one place a field one renames and the other does not shows.
func TestUserData_IsWhatInitsConfigParses(t *testing.T) {
	hostCfg, _ := hostNode(t)
	p := profile(t)
	orchHost, err := p.Node(joined(t))
	require.NoError(t, err)

	for name, n := range map[string]layout.Node{
		"a Node":               node(t, func(*layout.Node) {}),
		"a host":               hostCfg,
		"the host orch builds": orchHost,
	} {
		t.Run(name, func(t *testing.T) {
			data, err := n.UserData()
			require.NoError(t, err)
			cfg, err := config.Parse(data)
			require.NoError(t, err)
			assert.Contains(t, cfg.Metadata.Name, n.Name)
		})
	}
}

const guestOf = `{"kernels":{"naru":{"path":"/k/naru","sha256":"%[1]s"},"alt":{"path":"/k/alt","sha256":"%[1]s"}},"default_kernel":"naru",` +
	`"guest_initrd":{"path":"/g.cpio","sha256":"%[2]s"},"converter_initrd":{"path":"/c.cpio","sha256":"%[3]s"}}`

func mvmdFile(t *testing.T, n layout.Node) config.File {
	t.Helper()
	data, err := n.UserData()
	require.NoError(t, err)
	cfg, err := config.Parse(data)
	require.NoError(t, err)
	for _, f := range cfg.Spec.Files {
		if f.Path == layout.MvmdConfig {
			return f
		}
	}
	require.FailNow(t, "no mvmd configuration among spec.files")
	return config.File{}
}

func render(t *testing.T, f config.File, guest string) map[string]any {
	t.Helper()
	b, err := f.Render(func() ([]byte, error) { return []byte(guest), nil })
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc), string(b))
	return doc
}

// The stored configuration holds no digest of a guest file, so an upgrade that changes the guest
// manifest changes what mvmd is given with no edit of the configuration (ADR 0106).
func TestUserData_MvmdsGuestDigestsAreTheBootedImagesAndNeverStored(t *testing.T) {
	for name, n := range map[string]layout.Node{
		"a Node": node(t, func(*layout.Node) {}),
		"a host": func() layout.Node { h, _ := hostNode(t); return h }(),
	} {
		t.Run(name, func(t *testing.T) {
			f := mvmdFile(t, n)
			require.Equal(t, config.TemplateGuest, f.Template)
			assert.NotContains(t, f.Content, `"sha256": "0123`, "a digest in the stored file would be a second copy")

			before := render(t, f, fmt.Sprintf(guestOf, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)))
			after := render(t, f, fmt.Sprintf(guestOf, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)))

			fc := func(d map[string]any) map[string]any { return d["firecracker"].(map[string]any) }
			assert.Equal(t, strings.Repeat("2", 64), fc(before)["initrd_sha256"])
			assert.Equal(t, strings.Repeat("b", 64), fc(after)["initrd_sha256"], "the same stored file follows a changed guest initrd")
			assert.Equal(t, "/g.cpio", fc(after)["initrd"])
			assert.Equal(t, "naru", fc(after)["default_kernel"])
			kernels := fc(after)["kernels"].(map[string]any)
			assert.Len(t, kernels, 2, "every declared kernel")
			assert.Equal(t, strings.Repeat("a", 64), kernels["alt"].(map[string]any)["sha256"])
			conv := after["conversion"].(map[string]any)
			assert.Equal(t, "/k/naru", conv["kernel"], "the converter boots the default kernel")
			assert.Equal(t, strings.Repeat("a", 64), conv["kernel_sha256"])
			assert.Equal(t, "/c.cpio", conv["initrd"])
			assert.Equal(t, strings.Repeat("c", 64), conv["initrd_sha256"])
			assert.Equal(t, n.FirecrackerSHA256, fc(after)["binary_sha256"], "what is not the guest's stays in the file")
		})
	}
}
