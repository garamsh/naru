package service

// Cgroups is the cgroup hierarchy the services run in.
type Cgroups interface {
	// Prepare builds the hierarchy and enables the controllers down it.
	Prepare() error
	// Leaf returns the cgroup of the service name, creating it if absent.
	Leaf(name string) (Leaf, error)
}
