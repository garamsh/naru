//go:build qemu_real

package tests_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/dhcpserver"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

// The scenario below follows docs/operate/first-hosts.md: hosts that boot from one installation seed
// and have nothing of their own but a DHCP lease (ADR 0058, 0059). Node 1 is the control host; the
// hosts are started beside the cluster, from the seed, with a blank data disk and no network-config.

const (
	// joinRegistryPort is where node 1 serves the image the joined hosts pull: a host that joined has a
	// blank data disk, so it has no registry of its own with the test image.
	joinRegistryPort = "5001"
	// joinVMMiB and joinVMCPUs are the memory and CPUs a joined host has besides what it needs to boot.
	joinVMMiB  = 1024
	joinVMCPUs = 4
)

var (
	keyIDLine       = regexp.MustCompile(`(?m)^id\s+(\S+)`)
	enrollRefusal   = "orch refused the enrollment"
	joinApplied     = "join configuration applied revision=1"
	joinedHostIDRE  = regexp.MustCompile(`^h-[0-9a-f]{16}$`)
	harnessRegistry = "/run/harness/registry"
)

// pendingHost is an element of orch's GET /v1/pending.
type pendingHost struct {
	HostID   string `json:"hostId"`
	Code     string `json:"code"`
	Address  string `json:"address"`
	Hardware *struct {
		CPUs        int    `json:"cpus"`
		MemoryBytes uint64 `json:"memoryBytes"`
		Disks       []struct {
			Size       uint64 `json:"size"`
			Partitions int    `json:"partitions"`
		} `json:"disks"`
		Links []struct {
			MAC string `json:"mac"`
		} `json:"links"`
	} `json:"hardware"`
}

func (f *fleet) pending() []pendingHost {
	f.t.Helper()
	code, b := f.get(1, http.MethodGet, "/v1/pending")
	require.Equal(f.t, http.StatusOK, code, string(b))
	var ps []pendingHost
	require.NoError(f.t, json.Unmarshal(b, &ps))
	return ps
}

// pendingWith is the pending host whose NIC has the MAC, once orch lists it.
func (f *fleet) pendingWith(mac string) pendingHost {
	f.t.Helper()
	var found pendingHost
	eventually(f.t, "orch lists the host of "+mac+" as pending", func() bool {
		for _, p := range f.pending() {
			if p.Hardware == nil {
				continue
			}
			for _, l := range p.Hardware.Links {
				if l.MAC == mac {
					found = p
					return true
				}
			}
		}
		return false
	})
	return found
}

// serveDHCP runs the fleet's DHCP server for the test's length: it gives the hosts of the Node
// numbers their addresses, as an operator's reservations do on a real network.
func (f *fleet) serveDHCP(total int, indexes ...int) {
	f.t.Helper()
	router := netip.MustParseAddr(node.Gateway)
	var reservations []dhcpserver.Reservation
	for _, i := range indexes {
		mac, err := net.ParseMAC(node.QEMUNetwork(i, total).MAC)
		require.NoError(f.t, err)
		reservations = append(reservations, dhcpserver.Reservation{MAC: mac, Address: netip.MustParseAddr(nodeIP(i))})
	}
	srv, err := dhcpserver.New(dhcpserver.Config{
		Prefix: netip.MustParsePrefix(managementPrefix), Address: router, MAC: net.HardwareAddr{0x02, 0, 0, 0, 0xfe, 0xfe},
		Router: router, DNS: []netip.Addr{router}, NTP: []netip.Addr{router}, Lease: 24 * time.Hour, Reservations: reservations,
	})
	require.NoError(f.t, err)
	conn, err := dhcpserver.ListenMulticast(f.c.Mcast)
	require.NoError(f.t, err)
	ctx, cancel := context.WithCancel(f.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, conn)
	}()
	f.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// bootFromSeed starts the host of Node number index from the installation seed: a blank data disk and
// no network-config.
func (f *fleet) bootFromSeed(iso string, index, total int) *node.Node {
	f.t.Helper()
	nd, err := node.Start(node.Spec{
		Config: node.Config{Name: fmt.Sprintf("joined-%d", index), Index: index, Total: total, MemoryMiB: defaultNodeMiB + joinVMMiB, CPUs: joinVMCPUs},
		Disk:   filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: f.t.TempDir(), Mcast: f.c.Mcast, Seed: iso, BlankData: true,
	})
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		nd.Stop()
		f.t.Logf("console of %s:\n%s", nd.Name, nd.Console())
	})
	return nd
}

// TestFleet_TwoHostsInstalledFromOneSeedEnrollAreAdmittedWithTheirOwnIdentityAndRunAMicroVM boots two
// hosts from the same seed with an enrollment key, each of which is listed as pending with its own
// ID and hardware and the pairing code its console shows, admits them, and has a group run a
// microVM on one. A third host booted from the seed after the key was revoked is refused.
func TestFleet_TwoHostsInstalledFromOneSeedEnrollAreAdmittedWithTheirOwnIdentityAndRunAMicroVM(t *testing.T) {
	// The joined hosts have no registry with the test image; node 1 serves it to them over TLS, with a
	// certificate for its management address that the host profile names the authority of.
	certs, err := fixture.ServerCerts(net.ParseIP(nodeIP(1)))
	require.NoError(t, err)
	f := newFleet(t, fleetOpts{
		controls:        1,
		imageRepository: nodeIP(1) + ":" + joinRegistryPort + "/test/entry",
		services: []node.ExtraService{{
			Name: "naru166-joinregistry", Binary: path.Join(fixture.GuestRoot, "bin/naru-registry"),
			Args: []string{"-listen", nodeIP(1) + ":" + joinRegistryPort, "-dir", path.Join(fixture.GuestRoot, "registry"),
				"-cert", harnessRegistry + ".crt", "-key", harnessRegistry + ".key"},
		}},
		userData: func(_ node.Config, file string) error {
			if err := seed.Append(file, map[string]any{"path": harnessRegistry + ".crt", "content": string(certs.Cert)}, "spec", "files"); err != nil {
				return err
			}
			return seed.Append(file, map[string]any{"path": harnessRegistry + ".key", "mode": "0600", "content": string(certs.Key)}, "spec", "files")
		},
	})
	f.startOrch()

	// Each host boots from a seed of its own, which carries an enrollment key that enrolls one host
	// (ADR 0121).
	orchURL := "https://" + nodeIP(1) + ":" + orchPort
	seedFor := func(name string) (iso, keyID string) {
		created := f.must(f.orch("enroll-key", "create"), "enroll-key create")
		id, secret := keyIDLine.FindStringSubmatch(created), secretLine.FindStringSubmatch(created)
		require.NotNil(t, id, created)
		require.NotNil(t, secret, created)
		iso = filepath.Join(f.dir, name+".iso")
		f.must(nrun("host", "install-seed", "--orch-url", orchURL, "--orch-ca-sha256", f.caSHA, "--enroll-key", f.file(name+".key", secret[1]+"\n"), "--out", iso), "host install-seed")
		return iso, id[1]
	}

	// The host profile is what orch builds each host's configuration from: the Firecracker the image carries,
	// the pool each host's Machine prefix is taken from, the
	// registry the microVM's image is pulled from, and the management network.
	profile := filepath.Join(f.dir, "host-profile.yaml")
	f.must(nrun("gen", "host-profile", "--firecracker-sha256", inputs.FirecrackerSHA256,
		"--machine-pool", "10.77.0.0/16", "--orch-url", orchURL, "--orch-ca-sha256", f.caSHA,
		"--registry", "host="+nodeIP(1)+":"+joinRegistryPort+",ca="+f.file("registry-ca.pem", string(certs.CA)),
		"--host-management-prefix", managementPrefix, "--host-egress-interface", "eth0", "--host-tunnel-port", "51820", "--out", profile), "gen host-profile")
	f.must(f.orch("host", "profile", "set", "-f", profile), "host profile set")

	// Hosts 2 and 3 enroll with the key; host 4 boots after the key is revoked.
	const total = 4
	f.serveDHCP(total, 2, 3, 4)
	isoA, _ := seedFor("seed-2")
	isoB, _ := seedFor("seed-3")
	a, b := f.bootFromSeed(isoA, 2, total), f.bootFromSeed(isoB, 3, total)
	hosts := map[*node.Node]pendingHost{}
	for i, nd := range []*node.Node{a, b} {
		hosts[nd] = f.pendingWith(node.QEMUNetwork(i+2, total).MAC)
	}

	// While pending: each host has its own ID, its own address from the DHCP server and the hardware
	// it reported, and the code orch lists is the one its console shows.
	pa, pb := hosts[a], hosts[b]
	assert.Regexp(t, joinedHostIDRE, pa.HostID)
	assert.Regexp(t, joinedHostIDRE, pb.HostID)
	assert.NotEqual(t, pa.HostID, pb.HostID, "each host has an ID of its own")
	assert.NotEqual(t, pa.Code, pb.Code, "and a pairing code of its own")
	for nd, p := range hosts {
		eventually(t, nd.Name+" shows the pairing code orch lists", func() bool {
			return nd.Has(p.HostID, "waits for admission, pairing code "+p.Code)
		})
		require.NotNil(t, p.Hardware, nd.Name+" reported its hardware")
		assert.Equal(t, joinVMCPUs, p.Hardware.CPUs, nd.Name+": the CPUs of the VM")
		assert.Greater(t, p.Hardware.MemoryBytes, uint64(defaultNodeMiB)<<20/2, nd.Name+": the memory of the VM")
		blank := 0
		for _, d := range p.Hardware.Disks {
			if d.Size == node.DataDiskBytes && d.Partitions == 0 {
				blank++
			}
		}
		assert.Equal(t, 1, blank, nd.Name+": the blank data disk is among the disks it reported")
	}
	assert.True(t, a.Has("DHCPv4 lease", nodeIP(2), node.Gateway), "host 2 leased its reserved address, and the router and NTP server the server named")
	assert.True(t, b.Has("DHCPv4 lease", nodeIP(3), node.Gateway), "host 3 leased its reserved address, and the router and NTP server the server named")

	// A revoked key enrolls no host: a seed whose key is revoked before its host boots is refused at
	// the enrollment, which the two above, made the same way and not revoked, show was not for
	// another reason.
	isoC, keyC := seedFor("seed-4")
	f.must(f.orch("enroll-key", "revoke", keyC), "enroll-key revoke")
	c := f.bootFromSeed(isoC, 4, total)
	require.NoError(t, c.WaitFor(t.Context(), bootTimeout, enrollRefusal), "the third host is told the enrollment is refused")
	assert.Len(t, f.pending(), 2, "the refused host is not pending: only the two that enrolled before the revocation are")

	// Admission: each host gets its region and labels, applies the configuration orch built for it,
	// and boots again with it.
	ids := map[*node.Node]string{}
	for nd, p := range hosts {
		ids[nd] = f.admit(p.Code, node.HostRegion, "pool=seed")
		assert.Equal(t, p.HostID, ids[nd])
	}
	var prefixes []string
	for nd, id := range ids {
		require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, joinApplied), nd.Name+" applied its configuration")
		awaitRestarting(t, nd, nd.Name+" serves the host API with its admission certificate: orch hears from it", func() bool {
			for _, h := range f.hostList(1) {
				if h.ID == id {
					return !h.LastSeen.IsZero()
				}
			}
			return false
		})
		var prefix string
		for _, h := range f.hostList(1) {
			if h.ID == id {
				assert.Equal(t, node.HostRegion, h.Region)
				assert.Equal(t, "seed", h.Labels["pool"])
				prefix = h.MachinePrefix
			}
		}
		assert.NotEmpty(t, prefix, nd.Name+": GET /v1/hosts carries the Machine prefix orch allocated")
		assert.Regexp(t, id+`\s.*`+regexp.QuoteMeta(prefix), f.must(f.orch("host", "list"), "host list"), nd.Name+": naructl host list shows the prefix")
		prefixes = append(prefixes, prefix)
	}
	require.Len(t, prefixes, 2)
	assert.NotEqual(t, prefixes[0], prefixes[1], "each host has a Machine prefix of its own")
	assert.Empty(t, f.pending(), "nothing is pending once both are admitted")

	// A microVM: the group selects the joined hosts by region and label, and a member runs on one of them.
	f.must(f.orch("group", "apply", "-f", f.groupFile("seeded", 1, 256, "requiredLabels: {pool: seed}\n")), "apply seeded")
	eventually(t, "a member of the group runs on a joined host", func() bool {
		g, ok := f.group(1, "seeded")
		if !ok {
			return false
		}
		for host := range g.running() {
			for _, id := range ids {
				if host == id {
					return true
				}
			}
		}
		return false
	})
	require.NoError(t, f.node(1).WaitFor(t.Context(), bootTimeout, "served", inputs.Image.Digest), "the registry of node 1 served the image the joined host pulled")
}
