package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

const hostDoc = valid + `  host:
    region: eu-1
    labels:
      disk: ssd
      example.com/tier: gold
`

func TestParse_HostDerivesTheFileNaruHostdReads(t *testing.T) {
	cfg, err := config.Parse([]byte(hostDoc))
	require.NoError(t, err)

	require.NotNil(t, cfg.Spec.Host)
	require.Len(t, cfg.Spec.Files, 3)
	f := cfg.Spec.Files[2]
	assert.Equal(t, config.HostFile, f.Path)
	assert.JSONEq(t, `{"region":"eu-1","labels":{"disk":"ssd","example.com/tier":"gold"}}`, f.Content)

	again, err := config.Parse([]byte(hostDoc))
	require.NoError(t, err)
	assert.Equal(t, f.Content, again.Spec.Files[2].Content, "equal hosts give equal content, so an unchanged apply writes nothing")
}

func TestParse_ANodeWithoutHostWritesNoHostFile(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))
	require.NoError(t, err)
	assert.Nil(t, cfg.Spec.Host)
	assert.Len(t, cfg.Spec.Files, 2)
}

func TestParse_TheDerivedFileOfAStoredConfigurationFollowsSpecHost(t *testing.T) {
	// A stored configuration comes back with the derived file in spec.files, and a changed
	// label must replace it, not conflict with it.
	doc := strings.Replace(hostDoc, "    - path: /run/mvmd/token\n      content: secret\n",
		"    - path: /run/mvmd/token\n      content: secret\n    - path: /run/naru-hostd/host.json\n      content: stale\n", 1)
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	require.Len(t, cfg.Spec.Files, 3, "the file is replaced, not added to")
	assert.Contains(t, cfg.Spec.Files[2].Content, "eu-1")

	without := strings.Replace(doc, "  host:\n    region: eu-1\n    labels:\n      disk: ssd\n      example.com/tier: gold\n", "", 1)
	cfg, err = config.Parse([]byte(without))
	require.NoError(t, err)
	assert.Len(t, cfg.Spec.Files, 2, "without spec.host the derived file goes")
}

func TestParse_HostRefuses(t *testing.T) {
	tests := []struct{ name, from, to, field string }{
		{"region not a DNS label", "region: eu-1", "region: EU_1", "spec.host.region"},
		{"no region", "region: eu-1", "region: ''", "spec.host.region"},
		{"label value with a space", "disk: ssd", "disk: 'a b'", "spec.host.labels"},
		{"label key with two slashes", "disk: ssd", "a/b/c: x", "spec.host.labels"},
		{"label key too long", "disk: ssd", strings.Repeat("k", 64) + ": x", "spec.host.labels"},
		{"label key with an uppercase prefix", "disk: ssd", "Example.com/x: y", "spec.host.labels"},
		{"unknown field", "region: eu-1", "region: eu-1\n    zone: a", "spec.host.zone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, hostDoc, tt.from)
			_, err := config.Parse([]byte(strings.Replace(hostDoc, tt.from, tt.to, 1)))
			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
	// The control: the document the cases change is accepted.
	_, err := config.Parse([]byte(hostDoc))
	assert.NoError(t, err)
}
