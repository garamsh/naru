package service

import (
	"context"

	"github.com/garamsh/naru/lib/init/config"
)

// Launcher starts a service's process.
type Launcher interface {
	// Launch starts svc directly inside the cgroup directory cgroup. Whatever it runs before the
	// service starts (the setup steps) ends when ctx does; the service itself does not.
	Launch(ctx context.Context, svc config.Service, cgroup string) (Process, error)
}
