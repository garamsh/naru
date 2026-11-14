package mvmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// createRequestID is the request ID of every CreateMachine hostd makes. The Machine's name is the
// idempotency key of the host API, so a repeat is the same Machine under the same request ID, and
// mvmd answers it with the original outcome; other parameters under the name are a conflict.
const createRequestID = "naru-hostd"

// diskRequestID is the request ID of every CreateDisk, for the same reason.
const diskRequestID = "naru-hostd"

// dialTimeout bounds one attempt to reach mvmd's socket.
const dialTimeout = 5 * time.Second

// GRPC is mvmd over the Unix socket at Socket. It dials on the first call that needs a connection
// and again after a call found the connection broken, so mvmd restarting does not end hostd.
type GRPC struct {
	Socket string

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client v1alpha2.RuntimeServiceClient
}

// New returns a GRPC that has not dialled.
func New(socket string) *GRPC { return &GRPC{Socket: socket} }

func (g *GRPC) rpc(ctx context.Context) (v1alpha2.RuntimeServiceClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		return g.client, nil
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := runtimeapi.Dial(dctx, g.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	g.conn, g.client = conn, v1alpha2.NewRuntimeServiceClient(conn)
	return g.client, nil
}

// Close closes the connection.
func (g *GRPC) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return nil
	}
	err := g.conn.Close()
	g.conn, g.client = nil, nil
	return err
}

// fail maps a gRPC error to one of the package's errors, and drops the connection when it is
// broken so the next call dials again.
func (g *GRPC) fail(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalid, msg)
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrPrecondition, msg)
	case codes.Unavailable:
		_ = g.Close()
		return fmt.Errorf("%w: %s", ErrUnavailable, msg)
	}
	return err
}

// call runs f on a client with the context's own deadline, and maps its error.
func call[T any](ctx context.Context, g *GRPC, f func(v1alpha2.RuntimeServiceClient) (T, error)) (T, error) {
	var zero T
	c, err := g.rpc(ctx)
	if err != nil {
		return zero, err
	}
	out, err := f(c)
	if err != nil {
		return zero, g.fail(err)
	}
	return out, nil
}

func (g *GRPC) Info(ctx context.Context) (Info, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.GetRuntimeInfoResponse, error) {
		return c.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	})
	if err != nil {
		return Info{}, err
	}
	i := Info{
		CPUs: r.GetCapacity().GetCpus(), MemoryBytes: r.GetCapacity().GetMemoryBytes(),
		StateTotalBytes: r.GetCapacity().GetStateTotalBytes(), StateAvailableBytes: r.GetCapacity().GetStateAvailableBytes(),
	}
	if v := r.GetApiVersions(); len(v) > 0 {
		i.APIVersion = v[len(v)-1]
	}
	if d := r.GetDrivers(); len(d) > 0 {
		i.Driver = d[0].GetDriver().GetName() + " " + d[0].GetDriver().GetVersion()
	}
	i.MachinePrefixes = r.GetMachinePrefixes()
	for _, k := range r.GetKernels() {
		i.Kernels = append(i.Kernels, Kernel{Name: k.GetName(), Digest: k.GetDigest(), Default: k.GetIsDefault()})
	}
	return i, nil
}

func (g *GRPC) Pull(ctx context.Context, repository, digest string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.PullImageResponse, error) {
		return c.PullImage(ctx, &v1alpha2.PullImageRequest{Image: &v1alpha2.ImageRef{Repository: repository, Digest: digest}})
	})
	return err
}

func (g *GRPC) CreateDisk(ctx context.Context, id string, sizeBytes uint64) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.CreateDiskResponse, error) {
		return c.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: id, RequestId: diskRequestID, SizeBytes: sizeBytes})
	})
	return err
}

func (g *GRPC) CreateRawDisk(ctx context.Context, id string, sizeBytes uint64, contents []byte) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.CreateDiskResponse, error) {
		return c.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{
			DiskId: id, RequestId: diskRequestID, SizeBytes: sizeBytes,
			Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, Contents: contents,
		})
	})
	return err
}

func (g *GRPC) AttachDisk(ctx context.Context, machineID string, a Attachment) (Machine, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.AttachDiskResponse, error) {
		return c.AttachDisk(ctx, &v1alpha2.AttachDiskRequest{
			MachineId: machineID, Disk: &v1alpha2.DiskAttachment{DiskId: a.ID, ReadOnly: a.ReadOnly},
		})
	})
	if err != nil {
		return Machine{}, err
	}
	return machineFromProto(r.GetMachine()), nil
}

func (g *GRPC) DeleteDisk(ctx context.Context, id string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.DeleteDiskResponse, error) {
		return c.DeleteDisk(ctx, &v1alpha2.DeleteDiskRequest{DiskId: id})
	})
	return err
}

func (g *GRPC) ListDisks(ctx context.Context) ([]Disk, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ListDisksResponse, error) {
		return c.ListDisks(ctx, &v1alpha2.ListDisksRequest{})
	})
	if err != nil {
		return nil, err
	}
	var out []Disk
	for _, d := range r.GetDisks() {
		out = append(out, Disk{ID: d.GetId(), SizeBytes: d.GetSizeBytes(), Holder: d.GetHolderMachineId()})
	}
	return out, nil
}

func (g *GRPC) CreateMachine(ctx context.Context, cr Create) (Machine, error) {
	req := &v1alpha2.CreateMachineRequest{
		MachineId: cr.ID, RequestId: createRequestID, ImageDigest: cr.ImageDigest,
		Resources:     &v1alpha2.Resources{CpuCount: cr.CPU, MemoryBytes: cr.MemoryBytes},
		RootDiskBytes: cr.RootDiskBytes, NetworkPolicy: policyToProto(cr.Policy), RoutedPrefixes: cr.RoutedPrefixes,
	}
	if cr.BootSet != "" {
		req.OsBoot = &v1alpha2.OsBoot{BootSet: cr.BootSet, KernelCmdline: cr.Cmdline}
	}
	for _, d := range cr.Disks {
		req.Disks = append(req.Disks, &v1alpha2.DiskAttachment{DiskId: d.ID, ReadOnly: d.ReadOnly})
	}
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.CreateMachineResponse, error) {
		return c.CreateMachine(ctx, req)
	})
	if err != nil {
		return Machine{}, err
	}
	return machineFromProto(r.GetMachine()), nil
}

func (g *GRPC) StartMachine(ctx context.Context, id string) (Machine, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.StartMachineResponse, error) {
		return c.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
	})
	if err != nil {
		return Machine{}, err
	}
	return machineFromProto(r.GetMachine()), nil
}

func (g *GRPC) StopMachine(ctx context.Context, id string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.StopMachineResponse, error) {
		return c.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: id, GracePeriod: durationpb.New(10 * time.Second)})
	})
	return err
}

func (g *GRPC) DeleteMachine(ctx context.Context, id string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.DeleteMachineResponse, error) {
		return c.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: id})
	})
	return err
}

func (g *GRPC) InspectMachine(ctx context.Context, id string) (Machine, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.InspectMachineResponse, error) {
		return c.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: id})
	})
	if err != nil {
		return Machine{}, err
	}
	return machineFromProto(r.GetMachine()), nil
}

func (g *GRPC) ListMachines(ctx context.Context) ([]Machine, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ListMachinesResponse, error) {
		return c.ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	})
	if err != nil {
		return nil, err
	}
	out := make([]Machine, 0, len(r.GetMachines()))
	for _, m := range r.GetMachines() {
		out = append(out, machineFromProto(m))
	}
	return out, nil
}

func (g *GRPC) Renew(ctx context.Context, length time.Duration, number uint64) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.RenewDeadlineResponse, error) {
		return c.RenewDeadline(ctx, &v1alpha2.RenewDeadlineRequest{Length: durationpb.New(length), RenewalNumber: number})
	})
	return err
}

func (g *GRPC) InspectDeadline(ctx context.Context) (Deadline, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.InspectDeadlineResponse, error) {
		return c.InspectDeadline(ctx, &v1alpha2.InspectDeadlineRequest{})
	})
	if err != nil {
		return Deadline{}, err
	}
	s := r.GetStatus()
	return Deadline{
		Armed: s.GetPhase() == v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, LastNumber: s.GetLastRenewalNumber(),
		Remaining: s.GetRemaining().AsDuration(),
	}, nil
}

var stateNames = map[v1alpha2.MachineState]string{
	v1alpha2.MachineState_MACHINE_STATE_CREATED: StateCreated, v1alpha2.MachineState_MACHINE_STATE_RUNNING: StateRunning,
	v1alpha2.MachineState_MACHINE_STATE_PAUSED: StatePaused, v1alpha2.MachineState_MACHINE_STATE_STOPPED: StateStopped,
	v1alpha2.MachineState_MACHINE_STATE_FAULTED: StateFaulted,
}

func machineFromProto(m *v1alpha2.Machine) Machine {
	out := Machine{
		ID: m.GetId(), State: stateNames[m.GetState()], Failure: m.GetLastFailureReason(), Addresses: m.GetAddresses(),
		ImageDigest: m.GetImageDigest(), CPU: m.GetResources().GetCpuCount(), MemoryBytes: m.GetResources().GetMemoryBytes(),
		RootDiskBytes: m.GetRootDiskBytes(), Policy: policyFromProto(m.GetNetworkPolicy()),
		BootSet: m.GetOsBoot().GetBootSet(), Cmdline: m.GetOsBoot().GetKernelCmdline(), RoutedPrefixes: m.GetRoutedPrefixes(),
	}
	if out.State == "" {
		out.State = strings.ToLower(strings.TrimPrefix(m.GetState().String(), "MACHINE_STATE_"))
	}
	for _, d := range m.GetDisks() {
		out.Disks = append(out.Disks, Attachment{ID: d.GetDiskId(), ReadOnly: d.GetReadOnly()})
	}
	return out
}

var protocolNames = map[v1alpha2.Protocol]string{
	v1alpha2.Protocol_PROTOCOL_TCP: "tcp", v1alpha2.Protocol_PROTOCOL_UDP: "udp", v1alpha2.Protocol_PROTOCOL_ICMP: "icmp",
	v1alpha2.Protocol_PROTOCOL_ICMPV6: "icmpv6", v1alpha2.Protocol_PROTOCOL_ANY: "any",
}

func protocolValue(name string) v1alpha2.Protocol {
	for p, n := range protocolNames {
		if n == name {
			return p
		}
	}
	return v1alpha2.Protocol_PROTOCOL_UNSPECIFIED
}

func rulesToProto(rules []Rule) []*v1alpha2.NetworkRule {
	var out []*v1alpha2.NetworkRule
	for _, r := range rules {
		pr := &v1alpha2.NetworkRule{Prefix: r.Prefix, Protocol: protocolValue(r.Protocol)}
		for _, p := range r.Ports {
			pr.Ports = append(pr.Ports, &v1alpha2.PortRange{First: p.First, Last: p.Last})
		}
		out = append(out, pr)
	}
	return out
}

func rulesFromProto(rules []*v1alpha2.NetworkRule) []Rule {
	var out []Rule
	for _, r := range rules {
		hr := Rule{Prefix: r.GetPrefix(), Protocol: protocolNames[r.GetProtocol()]}
		for _, p := range r.GetPorts() {
			hr.Ports = append(hr.Ports, PortRange{First: p.GetFirst(), Last: p.GetLast()})
		}
		out = append(out, hr)
	}
	return out
}

func policyToProto(p Policy) *v1alpha2.NetworkPolicy {
	if len(p.Egress) == 0 && len(p.Ingress) == 0 {
		return nil
	}
	return &v1alpha2.NetworkPolicy{Egress: rulesToProto(p.Egress), Ingress: rulesToProto(p.Ingress)}
}

func policyFromProto(p *v1alpha2.NetworkPolicy) Policy {
	return Policy{Egress: rulesFromProto(p.GetEgress()), Ingress: rulesFromProto(p.GetIngress())}
}
