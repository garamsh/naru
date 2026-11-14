package apply_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/config"
)

// toObj returns cfg as a decoded document, the shape a request carries.
func toObj(t *testing.T, cfg config.NodeConfig) obj {
	t.Helper()
	data, err := yaml.Dump(cfg)
	require.NoError(t, err)
	var doc obj
	require.NoError(t, yaml.Load(data, &doc))
	return doc
}
