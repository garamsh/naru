package orchapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/garamsh/naru/lib/machineapi"
)

// Machine is a caller's Machine as GET /v1/machines shows it to an operator: whose it is, the
// Machine as the Machine API answers its caller, and the host it is bound to.
type Machine struct {
	Caller string `json:"caller"`
	machineapi.Machine
	// Binding is absent while the Machine is bound to no host.
	Binding *MachineBinding `json:"binding,omitempty"`
	// Reason is why a pending Machine waits, with the fleet's figures the caller's reason leaves
	// out, such as "no host has 16 GiB of memory free; the most free is 12 GiB".
	Reason string `json:"reason,omitempty"`
	// Move is the move in progress; absent when there is none.
	Move *MachineMove `json:"move,omitempty"`
}

// The steps of a move (ADR 0109 item 5), in their order. A move is recorded at requested, holds
// the target's capacity at reserved, and from committed runs on the target and completes forward.
const (
	MoveRequested = "requested"
	MoveReserved  = "reserved"
	MoveStopped   = "stopped"
	MoveCopied    = "copied"
	MoveCreated   = "created"
	MoveCommitted = "committed"
	MoveStarted   = "started"
)

// MachineMove is a Machine's move as the operator API shows it: who asked for it and when, the
// narrowing it asked for, the step it reached, the host it goes to once one is reserved, and why it
// waits, if it does.
type MachineMove struct {
	By        string     `json:"by"`
	Requested time.Time  `json:"requested"`
	Shape     *MoveShape `json:"shape,omitempty"`
	Step      string     `json:"step"`
	Source    string     `json:"source"`
	Target    string     `json:"target,omitempty"`
	// Cancelled is a cancellation asked for and not yet carried out.
	Cancelled bool `json:"cancelled,omitempty"`
	// AbandonVolumes is a move of a lost Machine with new, empty volumes.
	AbandonVolumes bool   `json:"abandonVolumes,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// MoveShape narrows where a move may go: a region, and labels a host must carry, beside the
// Machine's own shape. Neither may contradict it.
type MoveShape struct {
	Region string            `json:"region,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// MoveRequest is the body of POST /v1/machines/{caller}/{name}/move.
type MoveRequest struct {
	Shape *MoveShape `json:"shape,omitempty"`
	// AbandonVolumes moves a lost Machine, whose host was abandoned, with new, empty volumes in
	// place of the ones left on that host; it is refused for any other Machine (ADR 0109 item 6).
	AbandonVolumes bool `json:"abandonVolumes,omitempty"`
}

// AbandonedVolumes are the volumes of one Machine's epoch left behind, which orch keeps listed
// until an operator deletes them (ADR 0109 item 7). Role is RoleBound for the placement on an
// abandoned host, and RoleSource for a move's stopped source copy, left on a host that answers when
// the move's target was abandoned after the commit; Deleting is a delete of a source copy the
// reconciler has yet to carry out there.
type AbandonedVolumes struct {
	Caller   string    `json:"caller"`
	Name     string    `json:"name"`
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	Epoch    uint64    `json:"epoch"`
	Volumes  []string  `json:"volumes"`
	At       time.Time `json:"at"`
	Role     string    `json:"role"`
	Deleting bool      `json:"deleting,omitempty"`
}

// The roles of AbandonedVolumes.
const (
	RoleBound  = "bound"
	RoleSource = "source"
)

// AbandonedVolumes lists the volumes left on abandoned hosts, by caller, name and epoch.
func (c *Client) AbandonedVolumes(ctx context.Context) ([]AbandonedVolumes, error) {
	var out []AbandonedVolumes
	return out, c.do(ctx, http.MethodGet, "/v1/machines/abandoned-volumes", nil, nil, &out)
}

// DeleteAbandonedVolumes forgets the volumes the caller's Machine name left at the epoch on the
// abandoned host, which is to come back only reset or reinstalled.
func (c *Client) DeleteAbandonedVolumes(ctx context.Context, caller, name, host string, epoch uint64) error {
	return c.do(ctx, http.MethodDelete, machinePath(caller, name)+"/abandoned-volumes/"+url.PathEscape(host)+"/"+strconv.FormatUint(epoch, 10), nil, nil, nil)
}

// ByDrain is who a drain's moves are by.
const ByDrain = "drain"

// MachineBinding is the host a Machine runs on and the epoch of its placement there: its microVM
// on the host is m-<id>-<epoch> (ADR 0109).
type MachineBinding struct {
	Host  string `json:"host"`
	Epoch uint64 `json:"epoch"`
}

// Machines lists every caller's Machines, ordered by caller and name.
func (c *Client) Machines(ctx context.Context) ([]Machine, error) {
	var out []Machine
	return out, c.do(ctx, http.MethodGet, "/v1/machines", nil, nil, &out)
}

func machinePath(caller, name string) string {
	return "/v1/machines/" + url.PathEscape(caller) + "/" + url.PathEscape(name)
}

// Machine returns the caller's Machine name.
func (c *Client) Machine(ctx context.Context, caller, name string) (Machine, error) {
	var out Machine
	return out, c.do(ctx, http.MethodGet, machinePath(caller, name), nil, nil, &out)
}

// MoveMachine asks for the caller's Machine name to move to another host, and returns it with the
// move recorded. The move itself is the leader's, step by step.
func (c *Client) MoveMachine(ctx context.Context, caller, name string, r MoveRequest) (Machine, error) {
	var out Machine
	return out, c.do(ctx, http.MethodPost, machinePath(caller, name)+"/move", nil, r, &out)
}

// CancelMove asks for the move of the caller's Machine name to be abandoned before its commit.
func (c *Client) CancelMove(ctx context.Context, caller, name string) (Machine, error) {
	var out Machine
	return out, c.do(ctx, http.MethodDelete, machinePath(caller, name)+"/move", nil, nil, &out)
}
