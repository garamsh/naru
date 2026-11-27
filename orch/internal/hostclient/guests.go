package hostclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// execMargin is how much longer than its timeout an exec's call to the host may take: the host
// ends the command at the timeout and then reads its record.
const execMargin = time.Minute

// guest returns the API of the admitted host, read from the inventory: a guest call is made by the
// member that took it, which has no pass of its own.
func (m *MachineHosts) guest(ctx context.Context, hostID string) (*hostapi.Client, error) {
	admitted, err := reached(ctx, m.c.inv)
	if err != nil {
		return nil, fmt.Errorf("%w: read the inventory: %v", machines.ErrHostUnavailable, err)
	}
	for _, h := range admitted {
		if h.ID == hostID {
			return m.c.api(h), nil
		}
	}
	return nil, fmt.Errorf("%w: host %s is not admitted", machines.ErrHostUnavailable, hostID)
}

func (m *MachineHosts) Exec(ctx context.Context, host, microVM string, r hostapi.ExecRequest) (hostapi.ExecResult, error) {
	api, err := m.guest(ctx, host)
	if err != nil {
		return hostapi.ExecResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.Timeout())*time.Second+execMargin)
	defer cancel()
	out, err := api.Exec(ctx, microVM, r)
	return out, guestError(err)
}

func (m *MachineHosts) ReadFile(ctx context.Context, host, microVM, path string) ([]byte, error) {
	api, err := m.guest(ctx, host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	b, err := api.ReadFile(ctx, microVM, path)
	return b, guestError(err)
}

func (m *MachineHosts) WriteFile(ctx context.Context, host, microVM, path string, content []byte) error {
	api, err := m.guest(ctx, host)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return guestError(api.WriteFile(ctx, microVM, path, content))
}

// guestError is the host's answer to a guest call as the Machine domain names it. A microVM the
// host has not is one that does not run there; what is not a refusal of the request is the host
// not answering.
func guestError(err error) error {
	if err == nil {
		return nil
	}
	var he *hostapi.Error
	if !errors.As(err, &he) {
		return fmt.Errorf("%w: %v", machines.ErrHostUnavailable, err)
	}
	switch {
	case he.Status == http.StatusNotFound && he.Code == hostapi.CodeFileNotFound:
		return fmt.Errorf("%w: %s", machines.ErrFileNotFound, he.Message)
	case he.Status == http.StatusConflict && he.Code == hostapi.CodeNotRunning, he.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", machines.ErrNotRunning, he.Message)
	case he.Status == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %s", machines.ErrTooLarge, he.Message)
	case he.Status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s", machineapi.ErrInvalid, he.Message)
	}
	return fmt.Errorf("%w: %v", machines.ErrHostUnavailable, err)
}
