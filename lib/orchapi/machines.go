package orchapi

import (
	"context"
	"net/http"

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
}

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
