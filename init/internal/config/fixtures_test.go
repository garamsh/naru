package config_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/init/internal/config"
)

// The v0.3.0 release's test configurations carry throwaway keys the release
// already publishes (ADR 0016). They name no spec.api and no files[].restart.
// The *.golden.yaml files are the yaml.Dump of the NodeConfig the parser at
// commit 6aeca4c0d2f323d9b5db22cefa5603eda3b951b8, before spec.api and
// files[].restart, parsed from the same fixtures.
func TestParse_V030Fixtures(t *testing.T) {
	cases := []struct {
		file         string
		name         string
		controlPlane bool
		services     int
		authorities  int
		certificates int
	}{
		{"naru-node-1.test.yaml", "naru-node-1", true, 7, 1, 3},
		{"naru-node-2.test.yaml", "naru-node-2", false, 7, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			require.NoError(t, err)
			cfg, err := config.Parse(data)
			require.NoError(t, err)
			assert.Equal(t, tc.name, cfg.Metadata.Name)
			assert.Equal(t, tc.controlPlane, cfg.Spec.ControlPlane)
			assert.Len(t, cfg.Spec.Services, tc.services)
			assert.Len(t, cfg.Spec.PKI.Authorities, tc.authorities)
			assert.Len(t, cfg.Spec.PKI.Certificates, tc.certificates)
			golden, err := os.ReadFile("testdata/" + tc.name + ".test.golden.yaml")
			require.NoError(t, err)
			got, err := yaml.Dump(cfg)
			require.NoError(t, err)
			assert.Equal(t, string(golden), string(got), "the parse differs from the one the parser at 6aeca4c gave")
			assert.Nil(t, cfg.Spec.API)
			for _, f := range cfg.Spec.Files {
				assert.Empty(t, f.Restart)
			}
		})
	}
}
