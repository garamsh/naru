package service

import "github.com/garamsh/naru/init/internal/config"

// Launcher starts a service's process.
type Launcher interface {
	// Launch starts svc directly inside the cgroup directory cgroup.
	Launch(svc config.Service, cgroup string) (Process, error)
}
