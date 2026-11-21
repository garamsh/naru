package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	groupsetcd "github.com/garamsh/naru/orch/internal/groups/etcd"
	"github.com/garamsh/naru/orch/internal/groupsvc"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// eventLog is the inventory's event log: the engine's adapter appends to it and the API lists it.
type eventLog struct {
	inventory.Service
	events []orchapi.Event
	reads  int
}

func (l *eventLog) AppendEvent(_ context.Context, ev orchapi.Event) error {
	l.events = append(l.events, ev)
	return nil
}

func (l *eventLog) Events(context.Context, string) ([]orchapi.Event, error) {
	l.reads++
	return l.events, nil
}

type groupList struct {
	groupsvc.Service
	groups []orchapi.Group
}

func (g groupList) List(context.Context) ([]orchapi.Group, error) { return g.groups, nil }

func (g groupList) Get(_ context.Context, name string) (orchapi.Group, error) {
	for _, x := range g.groups {
		if x.Spec.Name == name {
			return x, nil
		}
	}
	return orchapi.Group{}, inventory.ErrNotFound
}

func running(id string) orchapi.GroupMember {
	return orchapi.GroupMember{ID: id, Phase: orchapi.PhaseRunning}
}

func TestGroupsShowTheLatestHoldOfAGroupThatHasFewerMembersThanItsCount(t *testing.T) {
	log := &eventLog{}
	rec := groupsetcd.NewEvents(log)
	at := time.Unix(1000, 0).UTC()
	for i, msg := range []string{"old reason", "host h-1: region eu-1 is not kr-seoul"} {
		require.NoError(t, rec.Record(t.Context(), groups.Event{Time: at.Add(time.Duration(i) * time.Minute), Kind: groups.EventHold, Group: "web", Message: msg}))
	}
	require.NoError(t, rec.Record(t.Context(), groups.Event{Time: at, Kind: groups.EventHold, Group: "other", Message: "other reason"}))
	h := &handler{Deps{
		Inventory: log,
		Groups: groupList{groups: []orchapi.Group{
			{Spec: orchapi.GroupSpec{Name: "web", Count: 2}, Members: []orchapi.GroupMember{running("web-1"), {ID: "web-0", Phase: orchapi.PhaseGone}}},
			{Spec: orchapi.GroupSpec{Name: "full", Count: 1}, Members: []orchapi.GroupMember{running("full-1")}},
			{Spec: orchapi.GroupSpec{Name: "other", Count: 1}, Members: []orchapi.GroupMember{}},
		}},
	}}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/groups/web", nil)
	req.SetPathValue("name", "web")
	h.groupGet(w, req, identity.Identity{})
	var g orchapi.Group
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &g))
	require.NotNil(t, g.Hold, "setup: the group is short and was held")
	require.Equal(t, "host h-1: region eu-1 is not kr-seoul", g.Hold.Message)
	require.Equal(t, at.Add(time.Minute), g.Hold.Time)

	w = httptest.NewRecorder()
	h.groupList(w, httptest.NewRequest(http.MethodGet, "/v1/groups", nil), identity.Identity{})
	var all []orchapi.Group
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &all))
	require.Len(t, all, 3)
	require.NotNil(t, all[0].Hold)
	require.Equal(t, "host h-1: region eu-1 is not kr-seoul", all[0].Hold.Message)
	require.Nil(t, all[1].Hold, "control: a group at its count shows no hold")
	require.NotNil(t, all[2].Hold)
	require.Equal(t, "other reason", all[2].Hold.Message, "each group shows its own hold")
}

func TestGroupsAtTheirCountDoNotReadTheLog(t *testing.T) {
	log := &eventLog{}
	h := &handler{Deps{Inventory: log, Groups: groupList{groups: []orchapi.Group{
		{Spec: orchapi.GroupSpec{Name: "full", Count: 1}, Members: []orchapi.GroupMember{running("full-1")}},
	}}}}
	w := httptest.NewRecorder()
	h.groupList(w, httptest.NewRequest(http.MethodGet, "/v1/groups", nil), identity.Identity{})
	require.Equal(t, http.StatusOK, w.Code)
	require.Zero(t, log.reads)
}
