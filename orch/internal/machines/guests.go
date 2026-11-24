package machines

import (
	"context"

	"github.com/garamsh/naru/lib/hostapi"
)

// Guests reaches a microVM's guest on its host. A call answers ErrNotRunning for a microVM that
// does not run or is not on the host, ErrFileNotFound for a file the guest has not, ErrTooLarge
// above hostapi.MaxGuestBytes, machineapi.ErrInvalid for a request the host refuses, and
// ErrHostUnavailable when the host does not answer.
type Guests interface {
	Exec(ctx context.Context, host, microVM string, r hostapi.ExecRequest) (hostapi.ExecResult, error)
	ReadFile(ctx context.Context, host, microVM, path string) ([]byte, error)
	WriteFile(ctx context.Context, host, microVM, path string, content []byte) error
}
