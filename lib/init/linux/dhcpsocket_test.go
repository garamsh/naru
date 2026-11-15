package linux

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// frameTo is the frame of a server's reply to the client port, the frame the filter admits.
func frameTo(payload string) []byte {
	return reply(buildFrame(dhcpMAC, netip.MustParseAddr("10.0.2.2"), netip.MustParseAddr("10.0.2.15"), []byte(payload)))
}

// withIPv4Options returns f with a 4-byte IPv4 option (header length 6), which moves the UDP header.
func withIPv4Options(f []byte) []byte {
	out := append([]byte(nil), f[:etherLen+ipv4Len]...)
	out = append(out, 1, 1, 1, 0)
	out = append(out, f[etherLen+ipv4Len:]...)
	out[etherLen] = 0x46
	binary.BigEndian.PutUint16(out[etherLen+2:], binary.BigEndian.Uint16(out[etherLen+2:])+4)
	return out
}

func TestDHCPFilter_AdmitsOnlyUnfragmentedUDPToThePortOfTheClient(t *testing.T) {
	raw, err := dhcpFilter()
	require.NoError(t, err)
	program, ok := bpf.Disassemble(raw)
	require.True(t, ok, "every instruction of the program decodes")
	vm, err := bpf.NewVM(program)
	require.NoError(t, err)

	// edit returns a reply with its IPv4 header and its UDP header changed by change.
	edit := func(f []byte, change func(ip, udp []byte)) []byte {
		out := append([]byte(nil), f...)
		ip := out[etherLen:]
		change(ip, ip[int(ip[0]&0x0f)*4:])
		return out
	}
	toPort := func(p byte) func(ip, udp []byte) { return func(_, udp []byte) { udp[3] = p } }
	tests := []struct {
		name  string
		frame []byte
		admit bool
	}{
		{"a reply to the client port", frameTo("x"), true},
		{"a reply behind IPv4 options", withIPv4Options(frameTo("x")), true},
		{"a datagram to the server port", edit(frameTo("x"), toPort(dhcp4.ServerPort)), false},
		{"a datagram to another port", edit(frameTo("x"), toPort(69)), false},
		{"a datagram to another port behind IPv4 options", edit(withIPv4Options(frameTo("x")), toPort(69)), false},
		{"TCP", edit(frameTo("x"), func(ip, _ []byte) { ip[9] = 6 }), false},
		{"a later fragment", edit(frameTo("x"), func(ip, _ []byte) { ip[6], ip[7] = 0x00, 0x08 }), false},
		{"the first fragment", edit(frameTo("x"), func(ip, _ []byte) { ip[6] = 0x20 }), false},
		{"a packet with Don't Fragment set", edit(frameTo("x"), func(ip, _ []byte) { ip[6] = 0x40 }), true},
		{"ARP", func() []byte {
			f := frameTo("x")
			binary.BigEndian.PutUint16(f[12:], 0x0806)
			return f
		}(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := vm.Run(tc.frame)

			require.NoError(t, err)
			assert.Equal(t, tc.admit, n > 0)
		})
	}
}

// inNamespace runs the test named child in this test binary again, in a user and network
// namespace of its own, where it holds the capabilities of a packet socket without the host's.
func inNamespace(t *testing.T, child string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+child+"$", "-test.v")
	cmd.Env = append(os.Environ(), "NARU_NAMESPACE_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "operation not permitted") && !strings.Contains(string(out), "--- FAIL") {
		t.Skipf("the kernel allows no unprivileged user namespace: %s", out)
	}
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "--- PASS: "+child, "the child ran and did not skip")
}

func TestPacketSocket_TheKernelQueuesOnlyDHCPRepliesForIt(t *testing.T) {
	inNamespace(t, "TestPacketSocketChild")
}

// queued reads every frame waiting on fd.
func queued(t *testing.T, fd int) (ports []uint16) {
	t.Helper()
	buf := make([]byte, 2048)
	for {
		n, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		if err != nil {
			require.ErrorIs(t, err, unix.EAGAIN)
			return ports
		}
		if n >= etherLen+ipv4Len+udpLen {
			ports = append(ports, binary.BigEndian.Uint16(buf[etherLen+ipv4Len+2:]))
		}
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	d, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(d)
}

func TestPacketSocketChild(t *testing.T) {
	if os.Getenv("NARU_NAMESPACE_CHILD") == "" {
		t.Skip("runs inside TestPacketSocket_TheKernelQueuesOnlyDHCPRepliesForIt")
	}
	all, err := netlinkLinks{}.List()
	require.NoError(t, err)
	require.Len(t, all, 1, "the namespace holds lo alone")
	lo := all[0]
	assert.True(t, lo.Loopback, "the kernel flags lo as loopback")
	require.NoError(t, netlinkLinks{}.SetUp(lo))

	fd, err := openPacketSocket(lo)
	require.NoError(t, err)
	defer unix.Close(fd)
	// The control sees every IPv4 frame of the link, so that a frame the filtered socket lacks is
	// one the filter dropped and not one that was never sent.
	control, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	require.NoError(t, err)
	defer unix.Close(control)
	require.NoError(t, unix.Bind(control, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: lo.Index}))

	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: dhcp4.ServerPort})
	require.NoError(t, err)
	defer server.Close()
	for _, port := range []int{dhcp4.ClientPort, 69, 5353, dhcp4.ClientPort} {
		_, err := server.WriteToUDP([]byte("payload"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		require.NoError(t, err)
	}
	time.Sleep(100 * time.Millisecond)

	seen := queued(t, control)
	assert.Contains(t, seen, uint16(69), "the control saw the datagram the filter drops")
	assert.Contains(t, seen, uint16(5353))
	got := queued(t, fd)
	require.NotEmpty(t, got, "the filter admits what is sent to the client port")
	for _, port := range got {
		assert.Equal(t, uint16(dhcp4.ClientPort), port, "the filtered socket queues nothing else")
	}

	before := openFDs(t)
	tr, err := openPacketTransport(lo)
	require.NoError(t, err)
	assert.Equal(t, before+1, openFDs(t), "an open transport holds a socket")
	require.NoError(t, tr.Close())
	assert.Equal(t, before, openFDs(t), "a closed transport holds none")
}
