package mvmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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
	case codes.DeadlineExceeded:
		// mvmd's own deadline, or the call's, which a status carries and the context error does not.
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
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
	for _, b := range r.GetBootSets() {
		i.BootSets = append(i.BootSets, BootSet{
			Name: b.GetName(), KernelDigest: b.GetKernelDigest(), InitrdDigest: b.GetInitrdDigest(), Origin: bootSetOrigin(b.GetOrigin()),
		})
	}
	return i, nil
}

// bootSetOrigin is the word the host API says of how a boot set came to be offered.
func bootSetOrigin(o v1alpha2.BootSetOrigin) string {
	switch o {
	case v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_CONFIGURED:
		return "configured"
	case v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED:
		return "added"
	}
	return ""
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

func (g *GRPC) CreateDiskFrom(ctx context.Context, id string, sizeBytes uint64, snapshotID string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.CreateDiskResponse, error) {
		return c.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: id, RequestId: diskRequestID, SizeBytes: sizeBytes, FromSnapshotId: snapshotID})
	})
	return err
}

func (g *GRPC) SnapshotDisk(ctx context.Context, snapshotID, diskID string) (DiskSnapshot, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.SnapshotDiskResponse, error) {
		return c.SnapshotDisk(ctx, &v1alpha2.SnapshotDiskRequest{DiskId: diskID, SnapshotId: snapshotID, RequestId: diskRequestID})
	})
	if err != nil {
		return DiskSnapshot{}, err
	}
	return diskSnapshotFromProto(r.GetDiskSnapshot()), nil
}

func (g *GRPC) ListDiskSnapshots(ctx context.Context) ([]DiskSnapshot, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ListDiskSnapshotsResponse, error) {
		return c.ListDiskSnapshots(ctx, &v1alpha2.ListDiskSnapshotsRequest{})
	})
	if err != nil {
		return nil, err
	}
	out := make([]DiskSnapshot, 0, len(r.GetDiskSnapshots()))
	for _, d := range r.GetDiskSnapshots() {
		out = append(out, diskSnapshotFromProto(d))
	}
	return out, nil
}

func (g *GRPC) DeleteDiskSnapshot(ctx context.Context, id string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.DeleteDiskSnapshotResponse, error) {
		return c.DeleteDiskSnapshot(ctx, &v1alpha2.DeleteDiskSnapshotRequest{SnapshotId: id})
	})
	return err
}

func diskSnapshotFromProto(d *v1alpha2.DiskSnapshot) DiskSnapshot {
	out := DiskSnapshot{ID: d.GetId(), DiskID: d.GetDiskId(), SizeBytes: d.GetSizeBytes(), AllocatedBytes: d.GetAllocatedBytes()}
	if d.GetCreatedAt() != nil {
		out.CreatedAt = d.GetCreatedAt().AsTime()
	}
	return out
}

func (g *GRPC) AttachDisk(ctx context.Context, machineID string, a Attachment) (Machine, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.AttachDiskResponse, error) {
		return c.AttachDisk(ctx, &v1alpha2.AttachDiskRequest{
			MachineId: machineID, Disk: &v1alpha2.DiskAttachment{DiskId: a.ID, ReadOnly: a.ReadOnly, MountPath: a.MountPath},
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

// createMachineRequest is mvmd's request for cr, under hostd's one request ID.
func createMachineRequest(cr Create) *v1alpha2.CreateMachineRequest {
	req := &v1alpha2.CreateMachineRequest{
		MachineId: cr.ID, RequestId: createRequestID, ImageDigest: cr.ImageDigest,
		Resources:     &v1alpha2.Resources{CpuCount: cr.CPU, MemoryBytes: cr.MemoryBytes},
		RootDiskBytes: cr.RootDiskBytes, NetworkPolicy: policyToProto(cr.Policy), RoutedPrefixes: cr.RoutedPrefixes,
		RestartOnBoot: cr.RestartOnBoot, Fenced: cr.Fenced, RequestedIpv4: cr.RequestedIPv4,
		Command: cr.Command, Args: cr.Args, User: cr.User, WorkingDir: cr.WorkingDir,
	}
	for _, name := range slices.Sorted(maps.Keys(cr.Env)) {
		req.Env = append(req.Env, &v1alpha2.EnvVar{Name: name, Value: cr.Env[name]})
	}
	for _, f := range cr.Files {
		req.Files = append(req.Files, &v1alpha2.File{Path: f.Path, Content: f.Content, Mode: f.Mode})
	}
	if cr.BootSet != "" {
		req.OsBoot = &v1alpha2.OsBoot{BootSet: cr.BootSet, KernelCmdline: cr.Cmdline}
	}
	for _, d := range cr.Disks {
		req.Disks = append(req.Disks, &v1alpha2.DiskAttachment{DiskId: d.ID, ReadOnly: d.ReadOnly, MountPath: d.MountPath})
	}
	return req
}

func (g *GRPC) CreateMachine(ctx context.Context, cr Create) (Machine, error) {
	req := createMachineRequest(cr)
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

func (g *GRPC) StopMachine(ctx context.Context, id string, grace time.Duration) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.StopMachineResponse, error) {
		return c.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: id, GracePeriod: durationpb.New(grace)})
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
	return deadlineFromProto(r.GetStatus()), nil
}

func (g *GRPC) RenewMachine(ctx context.Context, id string, length time.Duration, number uint64) (Deadline, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.RenewDeadlineResponse, error) {
		return c.RenewDeadline(ctx, &v1alpha2.RenewDeadlineRequest{
			Length: durationpb.New(length), RenewalNumber: number, MachineId: id,
		})
	})
	if err != nil {
		return Deadline{}, err
	}
	return deadlineFromProto(r.GetStatus()), nil
}

func (g *GRPC) InspectMachineDeadline(ctx context.Context, id string) (Deadline, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.InspectDeadlineResponse, error) {
		return c.InspectDeadline(ctx, &v1alpha2.InspectDeadlineRequest{MachineId: id})
	})
	if err != nil {
		return Deadline{}, err
	}
	return deadlineFromProto(r.GetStatus()), nil
}

var phaseNames = map[v1alpha2.DeadlinePhase]string{
	v1alpha2.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED: "notArmed", v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED: "armed",
	v1alpha2.DeadlinePhase_DEADLINE_PHASE_EXPIRED: "expired", v1alpha2.DeadlinePhase_DEADLINE_PHASE_STOPPED_EVERYTHING: "stoppedEverything",
}

func deadlineFromProto(s *v1alpha2.DeadlineStatus) Deadline {
	return Deadline{
		Armed: s.GetPhase() == v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, LastNumber: s.GetLastRenewalNumber(),
		Remaining: s.GetRemaining().AsDuration(), Phase: phaseNames[s.GetPhase()], Length: s.GetLength().AsDuration(),
	}
}

func (g *GRPC) SnapshotMachine(ctx context.Context, snapshotID, machineID string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.SnapshotMachineResponse, error) {
		return c.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{
			SnapshotId: snapshotID, RequestId: createRequestID, MachineId: machineID,
		})
	})
	return err
}

func (g *GRPC) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	r, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ListSnapshotsResponse, error) {
		return c.ListSnapshots(ctx, &v1alpha2.ListSnapshotsRequest{})
	})
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(r.GetSnapshots()))
	for _, s := range r.GetSnapshots() {
		sn := Snapshot{
			ID: s.GetId(), MachineID: s.GetMachineId(), ImageDigest: s.GetImageDigest(),
			Driver: strings.TrimSpace(s.GetDriver().GetName() + " " + s.GetDriver().GetVersion()), CPUModel: s.GetCpuModel(),
			Kernel: s.GetKernel(), RootDisk: s.GetRootDiskBytes(), SizeBytes: s.GetSizeBytes(),
		}
		if s.GetCreatedAt() != nil {
			sn.CreatedAt = s.GetCreatedAt().AsTime()
		}
		for _, d := range s.GetDisks() {
			sn.Disks = append(sn.Disks, SnapshotDisk{ID: d.GetDiskId(), ReadOnly: d.GetReadOnly(), SizeBytes: d.GetSizeBytes()})
		}
		out = append(out, sn)
	}
	return out, nil
}

func (g *GRPC) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.DeleteSnapshotResponse, error) {
		return c.DeleteSnapshot(ctx, &v1alpha2.DeleteSnapshotRequest{SnapshotId: id})
	})
	return err
}

func (g *GRPC) RestoreMachine(ctx context.Context, r Restore) (Machine, error) {
	resp, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.RestoreMachineResponse, error) {
		return c.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{
			MachineId: r.ID, RequestId: createRequestID, SnapshotId: r.SnapshotID,
			NetworkPolicy: policyToProto(r.Policy), RoutedPrefixes: r.RoutedPrefixes,
		})
	})
	if err != nil {
		return Machine{}, err
	}
	return machineFromProto(resp.GetMachine()), nil
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
		RestartOnBoot: m.GetRestartOnBoot(), Fenced: m.GetFenced(), EnvNames: m.GetEnvNames(), WorkingDir: m.GetWorkingDir(),
	}
	for _, f := range m.GetFiles() {
		out.Files = append(out.Files, FileInfo{Path: f.GetPath(), Mode: f.GetMode(), SizeBytes: f.GetSizeBytes()})
	}
	if m.GetFenceLapsedAt() != nil {
		out.FenceLapsedAt = m.GetFenceLapsedAt().AsTime()
	}
	if out.State == "" {
		out.State = strings.ToLower(strings.TrimPrefix(m.GetState().String(), "MACHINE_STATE_"))
	}
	for _, d := range m.GetDisks() {
		out.Disks = append(out.Disks, Attachment{ID: d.GetDiskId(), ReadOnly: d.GetReadOnly(), MountPath: d.GetMountPath()})
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

// bootSetChunk is the size of one message of an AddBootSet stream.
const bootSetChunk = 1 << 20

func (g *GRPC) AddBootSet(ctx context.Context, set BootSet, kernel, initrd io.Reader) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.AddBootSetResponse, error) {
		stream, err := c.AddBootSet(ctx)
		if err != nil {
			return nil, err
		}
		start := &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_Start{Start: &v1alpha2.AddBootSetStart{
			Name: set.Name, KernelDigest: set.KernelDigest, InitrdDigest: set.InitrdDigest,
		}}}
		if err := sendBootSet(stream, start, kernel, func(b []byte) *v1alpha2.AddBootSetRequest {
			return &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_KernelChunk{KernelChunk: b}}
		}, initrd, func(b []byte) *v1alpha2.AddBootSetRequest {
			return &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_InitrdChunk{InitrdChunk: b}}
		}); err != nil {
			// A send that fails with io.EOF means the server ended the stream: its status is the reason.
			if errors.Is(err, io.EOF) {
				_, err = stream.CloseAndRecv()
			}
			return nil, err
		}
		return stream.CloseAndRecv()
	})
	return err
}

// sendBootSet sends start, then the kernel's chunks, then the initrd's, each as wrapped.
func sendBootSet(stream v1alpha2.RuntimeService_AddBootSetClient, start *v1alpha2.AddBootSetRequest,
	kernel io.Reader, kernelMsg func([]byte) *v1alpha2.AddBootSetRequest,
	initrd io.Reader, initrdMsg func([]byte) *v1alpha2.AddBootSetRequest,
) error {
	if err := stream.Send(start); err != nil {
		return err
	}
	for _, part := range []struct {
		r   io.Reader
		msg func([]byte) *v1alpha2.AddBootSetRequest
	}{{kernel, kernelMsg}, {initrd, initrdMsg}} {
		buf := make([]byte, bootSetChunk)
		for {
			n, err := part.r.Read(buf)
			if n > 0 {
				if err := stream.Send(part.msg(buf[:n])); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("read the boot set's file: %w", err)
			}
		}
	}
	return nil
}

func (g *GRPC) RemoveBootSet(ctx context.Context, name string) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.RemoveBootSetResponse, error) {
		return c.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: name})
	})
	return err
}

// fileChunk is the size of one message of a WriteFile stream.
const fileChunk = 1 << 20

func (g *GRPC) Exec(ctx context.Context, machineID, execID string, command []string) (ExecRecord, error) {
	resp, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ExecResponse, error) {
		// The exec's own ID is its request ID: hostd never runs one twice.
		return c.Exec(ctx, &v1alpha2.ExecRequest{ExecId: execID, RequestId: execID, MachineId: machineID, Command: command})
	})
	if err != nil {
		return ExecRecord{}, err
	}
	return execRecordFromProto(resp.GetRecord()), nil
}

// ReadExecOutput's sink error reaches the caller as it is: fail passes on what is no gRPC status.
func (g *GRPC) ReadExecOutput(ctx context.Context, machineID, execID string, after uint64, sink func(Chunk) error) (ExecRecord, error) {
	rec, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.ExecRecord, error) {
		return v1alpha2.ReadExecOutput(ctx, c, machineID, execID, after, func(ch *v1alpha2.OutputChunk) error {
			return sink(Chunk{Stderr: ch.GetStream() == v1alpha2.OutputStream_OUTPUT_STREAM_STDERR, Offset: ch.GetOffset(), Data: ch.GetData()})
		})
	})
	if err != nil {
		return ExecRecord{}, err
	}
	return execRecordFromProto(rec), nil
}

func execRecordFromProto(r *v1alpha2.ExecRecord) ExecRecord {
	out := ExecRecord{Reason: r.GetReason(), ExitCode: r.GetExitCode(), TermSignal: r.GetTermSignal()}
	switch r.GetStatus() {
	case v1alpha2.ExecStatus_EXEC_STATUS_NOT_STARTED:
		out.Status = ExecNotStarted
	case v1alpha2.ExecStatus_EXEC_STATUS_CUT_SHORT:
		out.Status = ExecCutShort
	case v1alpha2.ExecStatus_EXEC_STATUS_EXITED:
		out.Status = ExecExited
	default:
		out.Status = ExecStarted
	}
	return out
}

func (g *GRPC) SignalExec(ctx context.Context, machineID, execID string, signal int32) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.SignalExecResponse, error) {
		return c.SignalExec(ctx, &v1alpha2.SignalExecRequest{MachineId: machineID, ExecId: execID, Signal: signal})
	})
	return err
}

// ReadFile's sink error reaches the caller as it is, as ReadExecOutput's does.
func (g *GRPC) ReadFile(ctx context.Context, machineID, path string, sink func([]byte) error) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (struct{}, error) {
		stream, err := c.ReadFile(ctx, &v1alpha2.ReadFileRequest{MachineId: machineID, Path: path})
		if err != nil {
			return struct{}{}, err
		}
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return struct{}{}, nil
			}
			if err != nil {
				return struct{}{}, err
			}
			if err := sink(msg.GetChunk()); err != nil {
				return struct{}{}, err
			}
		}
	})
	return err
}

func (g *GRPC) WriteFile(ctx context.Context, machineID, path string, content io.Reader) error {
	_, err := call(ctx, g, func(c v1alpha2.RuntimeServiceClient) (*v1alpha2.WriteFileResponse, error) {
		stream, err := c.WriteFile(ctx)
		if err != nil {
			return nil, err
		}
		if err := sendFile(stream, machineID, path, content); err != nil {
			// A send that fails with io.EOF means the server ended the stream: its status is the reason.
			if errors.Is(err, io.EOF) {
				_, err = stream.CloseAndRecv()
			}
			return nil, err
		}
		return stream.CloseAndRecv()
	})
	return err
}

// sendFile sends the start of a WriteFile, then content in chunks.
func sendFile(stream v1alpha2.RuntimeService_WriteFileClient, machineID, path string, content io.Reader) error {
	start := &v1alpha2.WriteFileRequest{Part: &v1alpha2.WriteFileRequest_Start{Start: &v1alpha2.WriteFileStart{MachineId: machineID, Path: path}}}
	if err := stream.Send(start); err != nil {
		return err
	}
	buf := make([]byte, fileChunk)
	for {
		n, err := content.Read(buf)
		if n > 0 {
			if err := stream.Send(&v1alpha2.WriteFileRequest{Part: &v1alpha2.WriteFileRequest_Chunk{Chunk: buf[:n]}}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the file's content: %w", err)
		}
	}
}
