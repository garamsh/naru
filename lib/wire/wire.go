// Package wire holds the values two parts of naru agree on without calling each other: where the
// data disk is and what it is called, the EFI system partition's GUID, the ports, the form of
// an enrollment key, the header of orch's fencing token and the protocols of an exposed port. Each is defined here once and imported by every Go user.
package wire

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The data disk and the programs the image carries.
const (
	// DataLabel and DataMount are the data disk's file system label and its mount point.
	DataLabel = "NARUDATA"
	DataMount = "/var/lib/naru"
	// ResetMarker is the file naru-hostd writes to ask init for a reset at its next boot, in init's
	// store directory on the data disk (ADR 0082). Both read this one path.
	ResetMarker = DataMount + "/config/reset.json"
	// MvmdStateDir is mvmd's state directory, on the data disk, whose file system has fs-verity for
	// the Image Store (ADR 0003).
	MvmdStateDir = DataMount + "/mvmd"
	// MvmdStorageMount is where the first storage disk is mounted. It holds what mvmd keeps on one
	// XFS with reflinks: the disks, the snapshots and the Machines' run directories (ADR 0073).
	MvmdStorageMount = DataMount + "/mvmd-storage"
	// MkfsPath is the mkfs.ext4 the image carries (ADR 0011), which init runs and mvmd's disk.mkfs names.
	MkfsPath = "/bin/mkfs.ext4"
	// GuestManifest is guest.json, which the image carries beside the guest kernels and initrds it
	// names (ADR 0010). A file of spec.files with `template: guest` is rendered from it at each write,
	// so the digests a configuration names are the booted image's own.
	GuestManifest = "/usr/lib/naru/guest/guest.json"
	// ESPGUID is the fixed partition GUID of the EFI system partition of the image's disk, which
	// image/host/disk/disk.env also holds as ESP_GUID (ADR 0020). The partition is found by it, not
	// by a device name.
	ESPGUID = "cb4162e7-9d1e-48df-b937-f5792071b950"
)

// The TCP ports of the APIs.
const (
	// HostAPIPort is the port naru-hostd serves the host API on (ADR 0028).
	HostAPIPort = "7443"
	// NodeAPIPort is the port init serves the Node API on by default (ADR 0017).
	NodeAPIPort = "50000"
	// MachineAPIPort is the port every orch member serves the Machine API on (ADR 0108 item 3).
	MachineAPIPort = "7600"
)

// What naru-hostd and naru-proxy share on a proxy host (ADR 0112 items 4 and 5).
const (
	// ProxyUID is the user and group naru-proxy runs as. The routes file is in its group.
	ProxyUID uint32 = 6003
	// ProxyRoutesDir holds ProxyRoutesFile, the table hostd writes for the proxy (hostapi.RoutesFile),
	// mode 0640 in ProxyUID's group. It is on the data disk, so a proxy that restarts forwards on
	// the table it had while that table is young enough.
	ProxyRoutesDir  = DataMount + "/proxy-routes"
	ProxyRoutesFile = ProxyRoutesDir + "/routes.json"
	// ProxyRunDir is the proxy's own directory, and ProxyRevisionFile in it the revision of the
	// table the proxy has loaded, in decimal, which hostd answers orch's push with.
	ProxyRunDir       = "/run/naru-proxy"
	ProxyRevisionFile = ProxyRunDir + "/revision"
	// ProxyHealthAddr is where the proxy answers GET /healthz, on the host's loopback.
	ProxyHealthAddr = "127.0.0.1:7480"
	// ProxyRouteMaxAge is the proxy's --route-max-age when spec.proxy.routeMaxAge is absent: how
	// long after hostd took a table the proxy forwards on it.
	ProxyRouteMaxAge = time.Hour
	// ProxyMaxRouteMaxAge is the longest --route-max-age a proxy may be given: orch waits a proxy's
	// max age out before it reuses an address, so a longer one would hold every Machine's delete.
	ProxyMaxRouteMaxAge = 24 * time.Hour
	// ProxyPort is the port the proxy terminates TLS on.
	ProxyPort = "443"
	// ProxyRedirectListen is where the proxy answers plain HTTP with a redirect to https on the
	// same name, and ProxyRedirectPort its port, which a proxy exposure's port 80 reaches.
	ProxyRedirectListen = "0.0.0.0:80"
	ProxyRedirectPort   = 80
)

// ProxyMaxConns bounds the client connections naru-proxy holds at once, and ProxyMaxPerSource
// those of one client address, so one client cannot take every slot. They are hostd's exposure
// proxy's figures.
const (
	ProxyMaxConns     = 4096
	ProxyMaxPerSource = 128
)

// Protocol is the transport of an exposed port, in upper case (TCP, UDP). Orch's
// API, the host API and naru-hostd take it in this one spelling and no other (ADR 0107).
type Protocol string

// The protocols an exposure carries.
const (
	ProtocolTCP Protocol = "TCP"
	ProtocolUDP Protocol = "UDP"
)

// Protocols returns every Protocol, in order.
func Protocols() []Protocol { return []Protocol{ProtocolTCP, ProtocolUDP} }

// Valid reports whether p is one of Protocols.
func (p Protocol) Valid() bool { return slices.Contains(Protocols(), p) }

// LeaderTokenHeader names the header in which orch's leader sends its fencing token with every
// mutating call to a host (ADR 0071). The value is the decimal token; a call without it is an
// operator's.
const LeaderTokenHeader = "X-Naru-Leader-Token"

// EnrollKeyPrefix starts every enrollment key orch issues; the rest is the hex of EnrollKeyBytes
// random bytes, in lowercase.
const (
	EnrollKeyPrefix = "naru-ek-"
	EnrollKeyBytes  = 32
)

// EnrollKey returns the enrollment key made of secret.
func EnrollKey(secret [EnrollKeyBytes]byte) string {
	return EnrollKeyPrefix + hex.EncodeToString(secret[:])
}

// IsEnrollKey reports whether k has the form EnrollKey returns.
func IsEnrollKey(k string) bool {
	rest, ok := strings.CutPrefix(k, EnrollKeyPrefix)
	if !ok || len(rest) != 2*EnrollKeyBytes {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil && rest == strings.ToLower(rest)
}

// proxySignature opens every PROXY protocol v2 header.
var proxySignature = [12]byte{'\r', '\n', '\r', '\n', 0, '\r', '\n', 'Q', 'U', 'I', 'T', '\n'}

const (
	proxyVersionCommand = 0x21 // version 2, command PROXY
	proxyIPv4TCP        = 0x11 // address family AF_INET, transport STREAM
	proxyIPv4Length     = 12   // two addresses and two ports
)

// ProxyHeader returns the PROXY protocol v2 header for a TCP connection over IPv4 from src to dst:
// the signature, the version and command, the family and transport, the length, and the addresses
// and ports. naru-hostd's exposures and naru-proxy write it before a backend's first byte.
func ProxyHeader(src, dst netip.AddrPort) ([]byte, error) {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	if !s.Is4() || !d.Is4() {
		return nil, errors.New("the PROXY header carries IPv4 addresses only")
	}
	b := make([]byte, 0, 16+proxyIPv4Length)
	b = append(b, proxySignature[:]...)
	b = append(b, proxyVersionCommand, proxyIPv4TCP)
	b = binary.BigEndian.AppendUint16(b, proxyIPv4Length)
	b = append(b, s.AsSlice()...)
	b = append(b, d.AsSlice()...)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	b = binary.BigEndian.AppendUint16(b, dst.Port())
	return b, nil
}
