package hostclient_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/machines"
	"github.com/garamsh/naru/orch/internal/netrec"
)

func abandonment() *orchapi.HostAbandonment {
	return &orchapi.HostAbandonment{By: "alice", At: time.Unix(1, 0), Fenced: "powered off at the PDU"}
}

func TestAnAbandonedHostIsInNoGroupHostListAndNoPeerList(t *testing.T) {
	up, gone := newHost(t), newHost(t)
	c := client(inventory{{ID: "a-up", Address: up.addr()}, {ID: "b-gone", Address: gone.addr(), Abandonment: abandonment()}})
	hosts, err := c.List(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	assert.Equal(t, "a-up", hosts[0].ID, "control: the other host is listed")
	assert.False(t, gone.saw("GET /v1/microvms"), "the abandoned host is not asked")

	inv := &invFake{hosts: []orchapi.Host{{ID: "h1", Address: "192.0.2.1:7443"}, {ID: "h2", Address: "192.0.2.2:7443", Abandonment: abandonment()}}}
	n := hostclient.NewNetHosts(inv, func(_, addr string) hostclient.NetAPI { return &api{addr: addr} })
	list, err := n.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []netrec.Host{{ID: "h1", Address: "192.0.2.1:7443"}}, list, "no host is given it as a peer")
}

func TestMachineHostsMarkAnAbandonedHostWithoutAskingIt(t *testing.T) {
	up, gone := newHost(t), newHost(t)
	_, hosts := machineHosts(t, inventory{{ID: "a-up", Address: up.addr()}, {ID: "b-gone", Address: gone.addr(), Region: "kr-seoul", Abandonment: abandonment()}})
	require.Len(t, hosts, 2)
	assert.True(t, hosts[0].Answering, "control: the other host answers")
	assert.False(t, hosts[0].Abandoned)
	assert.Equal(t, machines.Host{ID: "b-gone", Region: "kr-seoul", Abandoned: true}, hosts[1], "listed, so its Machines are marked lost")
	assert.False(t, gone.saw("GET /v1/host"), "it is not probed")
}
