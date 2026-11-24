package machines

import (
	"context"
	"fmt"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
)

// guest returns the host and microVM of the caller's Machine when it runs. The member that took
// the call reaches the host itself: a guest call is no state, and does not pass through the leader.
func (s *service) guest(ctx context.Context, caller, name string) (host, microVM string, err error) {
	r, _, err := s.store.Machine(ctx, caller, name)
	if err != nil {
		return "", "", notFound(err, "Machine", name)
	}
	if r.Status.Phase != machineapi.PhaseRunning || r.Binding == nil {
		return "", "", fmt.Errorf("%w: %s is %s", ErrNotRunning, name, r.Status.Phase)
	}
	if s.guests == nil {
		return "", "", fmt.Errorf("%w: this member reaches no host", ErrHostUnavailable)
	}
	return r.Binding.Host, r.Binding.MicroVM(r.ID), nil
}

func (s *service) Exec(ctx context.Context, caller, name string, req machineapi.ExecRequest) (machineapi.ExecResult, error) {
	if err := req.Check(); err != nil {
		return machineapi.ExecResult{}, err
	}
	host, vm, err := s.guest(ctx, caller, name)
	if err != nil {
		return machineapi.ExecResult{}, err
	}
	out, err := s.guests.Exec(ctx, host, vm, hostapi.ExecRequest{Command: req.Command, TimeoutSeconds: req.TimeoutSeconds})
	if err != nil {
		return machineapi.ExecResult{}, err
	}
	return machineapi.ExecResult{ExitCode: out.ExitCode, Signal: out.Signal, TimedOut: out.TimedOut, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

func (s *service) ReadFile(ctx context.Context, caller, name, path string) ([]byte, error) {
	if err := machineapi.CheckPath(path); err != nil {
		return nil, err
	}
	host, vm, err := s.guest(ctx, caller, name)
	if err != nil {
		return nil, err
	}
	return s.guests.ReadFile(ctx, host, vm, path)
}

func (s *service) WriteFile(ctx context.Context, caller, name, path string, content []byte) error {
	if err := machineapi.CheckPath(path); err != nil {
		return err
	}
	if len(content) > machineapi.MaxGuestBytes {
		return ErrTooLarge
	}
	host, vm, err := s.guest(ctx, caller, name)
	if err != nil {
		return err
	}
	return s.guests.WriteFile(ctx, host, vm, path, content)
}
