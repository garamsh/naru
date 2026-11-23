package ccm_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/node/internal/ccm"
)

const goodConfig = `{"orch":"https://o:7443","ca":"/ca","cert":"/c","key":"/k","network":"lab","pool":"lan"}`

func TestTheConfigNeedsEveryFieldAndAPoolWhileTheLoadBalancerIsOn(t *testing.T) {
	c, err := ccm.ParseConfig(strings.NewReader(goodConfig))
	require.NoError(t, err)
	require.True(t, c.LoadBalancerOn())
	for _, f := range []string{"orch", "ca", "cert", "key", "network"} {
		_, err := ccm.ParseConfig(strings.NewReader(strings.Replace(goodConfig, `"`+f+`":`, `"x`+f+`":`, 1)))
		require.Error(t, err, "without %s", f)
	}
	_, err = ccm.ParseConfig(strings.NewReader(strings.Replace(goodConfig, `,"pool":"lan"`, "", 1)))
	require.ErrorContains(t, err, "pool")
	c, err = ccm.ParseConfig(strings.NewReader(strings.Replace(goodConfig, `,"pool":"lan"`, `,"loadbalancer":false`, 1)))
	require.NoError(t, err, "no pool is needed with the service controller off")
	require.False(t, c.LoadBalancerOn())
}
