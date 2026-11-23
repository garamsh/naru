package config_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

const cniPath = "/run/cni/net.d/10-naru.conflist"

// nodeDoc is the valid document with a spec.node of the given lines.
func nodeDoc(lines ...string) string {
	return strings.Replace(valid, "spec:\n", "spec:\n  node:\n"+strings.Join(lines, "\n")+"\n", 1)
}

func cniFiles(cfg config.NodeConfig) []config.File {
	var out []config.File
	for _, f := range cfg.Spec.Files {
		if f.Path == cniPath {
			out = append(out, f)
		}
	}
	return out
}

func TestParse_SpecNodeWritesTheCNIConfigurationOfItsPodPrefixAndNoneWithoutOne(t *testing.T) {
	cfg, err := config.Parse([]byte(nodeDoc("    podPrefix: 10.64.1.0/26", "    cniConf: "+cniPath)))
	require.NoError(t, err)
	require.Len(t, cniFiles(cfg), 1)
	assert.Contains(t, cniFiles(cfg)[0].Content, `"subnet": "10.64.1.0/26"`)

	// The control: the same document without a prefix writes no file, and parsing the result again
	// keeps one entry.
	cfg, err = config.Parse([]byte(nodeDoc("    cniConf: " + cniPath)))
	require.NoError(t, err)
	assert.Empty(t, cniFiles(cfg))
}

func TestParse_RefusesAPodPrefixOrCNIPathThatIsNotValid(t *testing.T) {
	for name, lines := range map[string][]string{
		"host bits":    {"    podPrefix: 10.64.1.1/26"},
		"IPv6":         {"    podPrefix: fd00::/64"},
		"relative CNI": {"    cniConf: cni/10.conflist"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(nodeDoc(lines...)))
			require.Error(t, err)
			assert.Contains(t, config.Fields(err)[0], "spec.node.")
		})
	}
}

func TestApplySeedMeta_ThePodPrefixOfTheSeedReplacesTheUserDatasAndMakesTheFile(t *testing.T) {
	cfg, err := config.Parse([]byte(nodeDoc("    podPrefix: 10.64.9.0/26", "    cniConf: "+cniPath)))
	require.NoError(t, err)

	got, err := config.ApplySeedMeta(cfg, []byte("instance-id: w1-1\nlocal-hostname: w1-1\nnaru-pod-prefix: 10.64.1.0/26\n"))

	require.NoError(t, err)
	assert.Equal(t, netip.MustParsePrefix("10.64.1.0/26"), got.Spec.Node.PodPrefix)
	require.Len(t, cniFiles(got), 1, "one entry, not the user-data's and the seed's")
	assert.Contains(t, cniFiles(got)[0].Content, `"subnet": "10.64.1.0/26"`)
}

func TestApplySeedMeta_SeedWithoutThePrefixLeavesTheConfigurationAsItIs(t *testing.T) {
	cfg, err := config.Parse([]byte(nodeDoc("    podPrefix: 10.64.9.0/26", "    cniConf: "+cniPath)))
	require.NoError(t, err)
	for _, meta := range []string{"", "instance-id: w1-1\nlocal-hostname: w1-1\n"} {
		got, err := config.ApplySeedMeta(cfg, []byte(meta))
		require.NoError(t, err)
		assert.Equal(t, cfg, got)
	}
}

func TestApplySeedMeta_RefusesAPrefixThatIsNotOne(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))
	require.NoError(t, err)
	for _, meta := range []string{"naru-pod-prefix: nonsense", "naru-pod-prefix: 10.64.1.1/26", "naru-pod-prefix: [a"} {
		_, err := config.ApplySeedMeta(cfg, []byte(meta))
		assert.Error(t, err, meta)
	}
}
