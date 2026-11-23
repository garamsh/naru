package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
)

func group(network string, members ...orchapi.GroupMember) orchapi.Group {
	return orchapi.Group{Spec: orchapi.GroupSpec{Template: orchapi.GroupTemplate{Network: network}}, Members: members}
}

func TestNetworkMembersAreTheNetworksGroupMembersPlacedByTheirHosts(t *testing.T) {
	gs := []orchapi.Group{
		group("a", orchapi.GroupMember{ID: "cp-1", Host: "h1", Phase: orchapi.PhaseRunning}, orchapi.GroupMember{ID: "cp-0", Host: "h2", Phase: orchapi.PhaseRunning}),
		group("b", orchapi.GroupMember{ID: "other", Host: "h1", Phase: orchapi.PhaseRunning}),
	}
	ms := []exposures.Machine{
		{Name: "cp-1", Host: "h1", Address: "10.1.0.5", Running: true},
		{Name: "cp-0", Host: "h2", Address: "10.1.0.6", Running: true},
		{Name: "other", Host: "h1", Address: "10.2.0.5", Running: true},
	}
	hs := []orchapi.Host{
		{ID: "h1", Region: "kr", Labels: map[string]string{orchapi.ZoneLabel: "z1"}},
		{ID: "h2", Region: "kr"},
	}
	got := networkMembers("a", gs, ms, hs)
	require.Equal(t, []orchapi.NetworkMember{
		{Name: "cp-0", Host: "h2", Phase: "running", Reported: true, Running: true, Address: "10.1.0.6", Region: "kr"},
		{Name: "cp-1", Host: "h1", Phase: "running", Reported: true, Running: true, Address: "10.1.0.5", Region: "kr", Zone: "z1"},
	}, got, "only the network's own members, sorted, with their host's region and zone")
	require.Len(t, networkMembers("b", gs, ms, hs), 1, "control: the other network lists its own")
}

func TestAGoneMemberIsNotListedAndAMemberNoHostReportedIsNotRunning(t *testing.T) {
	gs := []orchapi.Group{group("a",
		orchapi.GroupMember{ID: "gone", Host: "h1", Phase: orchapi.PhaseGone},
		orchapi.GroupMember{ID: "away", Host: "h2", Phase: orchapi.PhaseRunning},
		orchapi.GroupMember{ID: "off", Host: "h1", Phase: orchapi.PhaseRunning},
	)}
	ms := []exposures.Machine{{Name: "off", Host: "h1", Address: "10.1.0.7", Running: false}}
	got := networkMembers("a", gs, ms, nil)
	require.Equal(t, []orchapi.NetworkMember{
		{Name: "away", Host: "h2", Phase: "running"},
		{Name: "off", Host: "h1", Phase: "running", Reported: true, Address: "10.1.0.7"},
	}, got, "gone is absent; away (its host silent) stays, not running; off is stopped")
}
