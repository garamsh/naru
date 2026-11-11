package service

// Leaf is the cgroup of one service.
type Leaf interface {
	// Path is the cgroup's directory.
	Path() string
	// Kill kills every process in the cgroup, and in no other, and returns
	// once the cgroup is empty.
	Kill() error
	// Remove deletes the empty cgroup.
	Remove() error
}
