package probe

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"

	"github.com/garamsh/naru/tooling/probe/internal/proc"
)

// RunningState is the name of the state mvmd reports for a running Machine.
const RunningState = "MACHINE_STATE_RUNNING"

// Mvmd is the Runtime of a real mvmd, over the Unix socket at Socket.
type Mvmd struct {
	Socket string
	// MemoryBytes and DiskBytes are the memory of the Machine and the size of its root disk.
	MemoryBytes, DiskBytes uint64
}

func (m Mvmd) client(ctx context.Context) (v1alpha2.RuntimeServiceClient, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := runtimeapi.Dial(ctx, m.Socket)
	if err != nil {
		return nil, nil, err
	}
	return v1alpha2.NewRuntimeServiceClient(conn), func() { _ = conn.Close() }, nil
}

// Ready lists the Machines on a new connection.
func (m Mvmd) Ready(ctx context.Context) error {
	c, closeConn, err := m.client(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	_, err = c.ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	return err
}

// Arm arms the fencing deadline for an hour.
func (m Mvmd) Arm(ctx context.Context) error {
	c, closeConn, err := m.client(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	_, err = c.RenewDeadline(ctx, &v1alpha2.RenewDeadlineRequest{Length: durationpb.New(time.Hour), RenewalNumber: 1})
	return err
}

// Pull pulls the image by digest.
func (m Mvmd) Pull(ctx context.Context, repository, digest string) error {
	c, closeConn, err := m.client(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	// One attempt is bounded, so that the probe retries and reports the last error.
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	_, err = c.PullImage(ctx, &v1alpha2.PullImageRequest{Image: &v1alpha2.ImageRef{Repository: repository, Digest: digest}})
	return err
}

// CreateAndStart creates the Machine from the image and starts it.
func (m Mvmd) CreateAndStart(ctx context.Context, id, imageDigest string) error {
	c, closeConn, err := m.client(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	if _, err := c.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId: id, RequestId: "create-" + id, ImageDigest: imageDigest,
		Resources:     &v1alpha2.Resources{CpuCount: 1, MemoryBytes: m.MemoryBytes},
		RootDiskBytes: m.DiskBytes,
	}); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err := c.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id}); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// States returns the state Inspect and List report for the Machine.
func (m Mvmd) States(ctx context.Context, id string) (string, string, error) {
	c, closeConn, err := m.client(ctx)
	if err != nil {
		return "", "", err
	}
	defer closeConn()
	inspected, err := c.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: id})
	if err != nil {
		return "", "", err
	}
	listed, err := c.ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	if err != nil {
		return "", "", err
	}
	for _, l := range listed.GetMachines() {
		if l.GetId() == id {
			return inspected.GetMachine().GetState().String(), l.GetState().String(), nil
		}
	}
	return "", "", fmt.Errorf("List does not have machine %s", id)
}

// System is the Host of the real Node.
type System struct{ proc.Reader }

// Kill sends SIGKILL to pid.
func (System) Kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
