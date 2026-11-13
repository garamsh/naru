package node

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_RoutesNoMasqueradeAndNetEchoAreInTheUserData(t *testing.T) {
	c := cfg(1, 2, true, false)
	c.Routes = []Route{{To: "10.77.2.0/24", Via: "10.78.0.2"}}
	c.NoMasquerade = []string{"10.77.2.0/24"}
	c.NetEcho = []string{"-listen", "10.77.1.2:9000"}

	n := parse(t, c)

	assert.Equal(t, []route{{To: "10.77.2.0/24", Via: "10.78.0.2"}}, n.Spec.Network.Routes)
	assert.Contains(t, names(n), "netecho")
	var mvmd struct{ Network map[string]any }
	require.NoError(t, json.Unmarshal([]byte(n.Spec.Files[0].Content), &mvmd))
	assert.Equal(t, []any{"10.77.2.0/24"}, mvmd.Network["no_masquerade"])
}

func TestConfig_WithoutThemTheUserDataHasNone(t *testing.T) {
	n := parse(t, cfg(1, 2, true, false))

	assert.Empty(t, n.Spec.Network.Routes)
	assert.NotContains(t, names(n), "netecho")
	var mvmd struct{ Network map[string]any }
	require.NoError(t, json.Unmarshal([]byte(n.Spec.Files[0].Content), &mvmd))
	assert.NotContains(t, mvmd.Network, "no_masquerade")
}
