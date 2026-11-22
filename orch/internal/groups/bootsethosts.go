package groups

import "context"

// BootSetHosts is the host API as the boot set reconciler needs it.
type BootSetHosts interface {
	// List returns every admitted host with the boot sets it offers in NodeVersions.
	List(ctx context.Context) ([]Host, error)
	// AddBootSet streams the set's files into the host's boot sets; the same set again is not an
	// error. It must not be called with a set that has not come from a ReleaseStore.
	AddBootSet(ctx context.Context, hostID string, set BootSet) error
}
