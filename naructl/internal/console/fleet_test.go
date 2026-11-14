package console_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/naructl/internal/console"
)

type source struct {
	vms    []orchapi.MicroVM
	groups []orchapi.Group
	err    error
}

func (s source) MicroVMs(context.Context) ([]orchapi.MicroVM, error) { return s.vms, s.err }
func (s source) Groups(context.Context) ([]orchapi.Group, error)     { return s.groups, s.err }

func TestFleet_MicroVMsAreThoseOfTheAskedHost(t *testing.T) {
	f := console.NewFleet(source{vms: []orchapi.MicroVM{
		{Host: "a", Name: "web-1", State: "running", Group: "web", Image: "reg/web@sha256:aa", Network: "lab"},
		{Host: "b", Name: "web-2", State: "stopped", Group: "web"},
		{Host: "a", Name: "adhoc", State: "running", BootSet: "node-1.2"},
	}})

	got, err := f.MicroVMs(t.Context(), "a")
	require.NoError(t, err)
	assert.Equal(t, []console.MicroVM{{Name: "web-1", State: "running", Group: "web", Image: "reg/web@sha256:aa", Network: "lab"}, {Name: "adhoc", State: "running", Image: "node-1.2"}}, got)

	got, err = f.MicroVMs(t.Context(), "none")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFleet_GroupsCountTheRunningMembersAsReady(t *testing.T) {
	f := console.NewFleet(source{groups: []orchapi.Group{{
		Spec: orchapi.GroupSpec{Name: "db", Count: 3, Stateful: true},
		Members: []orchapi.GroupMember{
			{ID: "db-1", Phase: orchapi.PhaseRunning}, {ID: "db-2", Phase: orchapi.PhaseRunning}, {ID: "db-3", Phase: orchapi.PhasePending},
		},
	}}})

	got, err := f.Groups(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []console.Group{{Name: "db", Count: 3, Ready: 2, Stateful: true}}, got)
}

func TestFleet_ASourceFailureIsReturned(t *testing.T) {
	f := console.NewFleet(source{err: errors.New("orch down")})
	_, err := f.MicroVMs(t.Context(), "a")
	require.Error(t, err)
	_, err = f.Groups(t.Context())
	require.Error(t, err)
}
