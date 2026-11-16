package hostapi

import (
	"context"
	"net/http"
)

// The scopes of a reset (ADR 0082). A scope is what the reset removes from the host's data disk,
// where the host keeps its stored configuration, its identity and its state.
const (
	// ResetStore removes the stored configuration: the host runs its base again, as revision 0.
	ResetStore = "store"
	// ResetData removes everything on the data disk except the stored configuration.
	ResetData = "data"
	// ResetAll blanks the data disk: the host formats it again on its next boot, as on its first.
	ResetAll = "all"
)

// CodeRevokedPair is the Code of the 409 a reset of scope data or all is refused with when the
// pair the host's configuration names, which serves again once the disk is cleared, is revoked
// (ADR 0082): the host would be refused its own clients after it rebooted.
const CodeRevokedPair = "revoked-pair"

// ResetRequest is the body of POST /v1/reset. Confirm must be the host's ID, which GET
// /v1/maintenance and the host's enrollment name, so that a reset aimed at the wrong host is refused.
type ResetRequest struct {
	Scope   string `json:"scope"`
	Confirm string `json:"confirm"`
}

// ResetResponse is the 202 the host answers a reset with: the reset is recorded and the host is
// rebooting to carry it out.
type ResetResponse struct {
	Scope string `json:"scope"`
}

// Maintenance is the body of GET /v1/maintenance, which only a host in maintenance mode serves: the
// host's ID, which a reset confirms, and why the boot was refused.
type Maintenance struct {
	HostID string `json:"hostId"`
	Reason string `json:"reason"`
}

// Reset asks the host to reset its disk by scope and reboot. The host answers once the request is
// recorded and before it reboots, and carries the reset out at its next boot, before it reads its
// stored configuration.
func (c *Client) Reset(ctx context.Context, r ResetRequest) (ResetResponse, error) {
	var out ResetResponse
	err := c.do(ctx, http.MethodPost, "/v1/reset", r, &out)
	return out, err
}

// Maintenance returns what a host in maintenance mode reports. A host that is not in it answers 404.
func (c *Client) Maintenance(ctx context.Context) (Maintenance, error) {
	var m Maintenance
	err := c.do(ctx, http.MethodGet, "/v1/maintenance", nil, &m)
	return m, err
}
