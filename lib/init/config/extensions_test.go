package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

func TestParse_ReadsSpecExtensions(t *testing.T) {
	cfg, err := config.Parse([]byte(daemonsDoc(t)))

	require.NoError(t, err)
	assert.Equal(t, []config.Extension{{Name: "naru-orch", SHA256: orchDigest}}, cfg.Spec.Extensions)
	ext, ok := cfg.Spec.Extension("naru-orch")
	require.True(t, ok)
	assert.Equal(t, "/usr/lib/naru/extensions/naru-orch", ext.Dir())
	_, ok = cfg.Spec.Extension("other")
	assert.False(t, ok)
}

func TestParse_RefusesAnInvalidExtensionEntry(t *testing.T) {
	entry := "    - name: naru-orch\n      sha256: " + orchDigest + "\n"
	tests := []struct {
		name  string
		to    string
		field string
	}{
		{"a name with capitals", "    - name: Naru\n      sha256: " + orchDigest + "\n", "spec.extensions[0].name"},
		{"a digest that is not hex", "    - name: naru-orch\n      sha256: xyz\n", "spec.extensions[0].sha256"},
		{"an upper-case digest", "    - name: naru-orch\n      sha256: " + strings.ToUpper(orchDigest) + "\n", "spec.extensions[0].sha256"},
		{"a name listed twice", entry + "    - name: naru-orch\n      sha256: " + strings.Repeat("a", 64) + "\n", "spec.extensions[1].name"},
		{"a digest listed twice", entry + "    - name: other\n      sha256: " + orchDigest + "\n", "spec.extensions[1].sha256"},
	}
	doc := daemonsDoc(t)
	_, err := config.Parse([]byte(doc))
	require.NoError(t, err, "the control: the document the cases change is accepted")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(strings.Replace(doc, entry, tt.to, 1)))
			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
}

func TestParse_OrchAndSignerNeedTheNaruOrchExtension(t *testing.T) {
	doc := daemonsDoc(t)
	without := strings.Replace(doc, "  extensions:\n    - name: naru-orch\n      sha256: "+orchDigest+"\n", "", 1)
	require.NotEqual(t, doc, without)

	_, err := config.Parse([]byte(without))

	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.extensions")
}

func TestParse_AServiceFromAnExtensionNeedsItsEntry(t *testing.T) {
	doc := `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  services:
    - name: tool
      binary: /usr/lib/naru/extensions/tools/bin/tool
`
	_, err := config.Parse([]byte(doc))
	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.services[0].binary")

	with := strings.Replace(doc, "  services:\n", "  extensions:\n    - name: tools\n      sha256: "+orchDigest+"\n  services:\n", 1)
	cfg, err := config.Parse([]byte(with))
	require.NoError(t, err)
	assert.Equal(t, "tools", config.ExtensionOf(cfg.Spec.Services[0]))
	assert.Empty(t, config.ExtensionOf(config.Service{Binary: "/bin/other"}))
}
