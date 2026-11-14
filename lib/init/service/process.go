package service

import "syscall"

// Process is a started service process.
type Process interface {
	// Wait blocks until the process has exited and returns nil for a clean exit.
	Wait() error
	// PID returns the process ID.
	PID() int
	// Signal sends sig to the process.
	Signal(sig syscall.Signal) error
}
