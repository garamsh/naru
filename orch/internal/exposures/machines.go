package exposures

import "context"

// Machine is a microVM of a network as the hosts report it: the name it was made under, the host
// that runs it, its first address and whether it runs.
type Machine struct {
	Name    string
	Host    string
	Address string
	Running bool
}

// Machines lists the microVMs of a network on every host that answers. A host that does not answer
// contributes none, so a caller must not take a missing machine for a gone one.
type Machines interface {
	Machines(ctx context.Context, network string) ([]Machine, error)
}
