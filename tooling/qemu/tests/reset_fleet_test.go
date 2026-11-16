//go:build qemu_real

package tests_test

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

// The tests of the reset and of maintenance mode (ADR 0082). A reset is carried out at the next boot, and
// QEMU runs with -no-reboot, so a host that reboots for one ends its QEMU process, which is what each test
// waits on, and the Node is then started again on its disks, as a hypervisor does.

var hostIDLine = regexp.MustCompile(`"hostId": "([^"]+)"`)

// hostIDOf is the ID the host API at file reports, which a reset confirms.
func hostIDOf(t *testing.T, file string) string {
	t.Helper()
	r := hctl(file, []string{"host"})
	require.NoError(t, r.Err, r.refusal())
	m := hostIDLine.FindStringSubmatch(r.Stdout)
	require.NotNil(t, m, r.Stdout)
	return m[1]
}

func TestNode_AResetOfTheStoreReturnsTheNodeToRevisionZeroAndAResetOfTheDataKeepsTheStoredConfiguration(t *testing.T) {
	c := hostCluster(t)
	nd, admin, host := c.Nodes[0], c.API.Admin, c.Host.Provisioner
	id := hostIDOf(t, host)
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	require.Equal(t, 1, revisionOf(t, admin), "the configuration is stored as revision 1")
	stored := storeOf(t, nd)

	// The control: a reset that names another host's ID is refused and the Node does not reboot.
	wrong := tool.Run("reset", "--config", host, "--scope", "store", "--confirm", id+"-other")
	require.Error(t, wrong.Err, "a reset aimed at another host is refused")
	select {
	case <-nd.Done():
		t.Fatal("a refused reset rebooted the Node")
	case <-time.After(5 * time.Second):
	}

	// Scope data removes everything on the data disk but the stored configuration.
	r := tool.Run("reset", "--config", host, "--scope", "data", "--confirm", id)
	require.NoError(t, r.Err, r.Stderr)
	awaitRebootAndRestart(t, nd, admin)
	assert.Equal(t, 1, revisionOf(t, admin), "a reset of the data leaves the stored configuration")
	assert.Equal(t, stored, storeOf(t, nd))

	// Scope store removes the stored configuration: the base runs again, as revision 0.
	r = tool.Run("reset", "--config", host, "--scope", "store", "--confirm", id)
	require.NoError(t, r.Err, r.Stderr)
	awaitRebootAndRestart(t, nd, admin)
	assert.Equal(t, 0, revisionOf(t, admin), "the host is back at revision 0")
}

// joinedHost boots the host of Node 2 from the installation seed beside a control host, admits it and
// waits until orch hears from it with the configuration orch built, as the join test does.
func joinedHost(t *testing.T) (f *fleet, nd *node.Node, id string) {
	t.Helper()
	certs, err := fixture.ServerCerts(net.ParseIP(nodeIP(1)))
	require.NoError(t, err)
	f = newFleet(t, fleetOpts{
		controls:        1,
		imageRepository: nodeIP(1) + ":" + joinRegistryPort + "/test/entry",
		services: []node.ExtraService{{
			Name: "naru55-joinregistry", Binary: path.Join(fixture.GuestRoot, "bin/naru-registry"),
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
	created := f.must(f.orch("enroll-key", "create"), "enroll-key create")
	secret := secretLine.FindStringSubmatch(created)
	require.NotNil(t, secret, created)
	orchURL := "https://" + nodeIP(1) + ":" + orchPort
	iso := filepath.Join(f.dir, "seed.iso")
	f.must(nrun("host", "install-seed", "--orch-url", orchURL, "--orch-ca-sha256", f.caSHA, "--enroll-key", f.file("enroll.key", secret[1]+"\n"), "--out", iso), "host install-seed")
	guest, err := json.Marshal(inputs.Guest)
	require.NoError(t, err)
	profile := filepath.Join(f.dir, "host-profile.yaml")
	f.must(nrun("gen", "host-profile", "--guest", f.file("guest.json", string(guest)), "--firecracker-sha256", inputs.FirecrackerSHA256,
		"--machine-pool", "10.77.0.0/16", "--orch-url", orchURL, "--orch-ca-sha256", f.caSHA,
		"--registry", "host="+nodeIP(1)+":"+joinRegistryPort+",ca="+f.file("registry-ca.pem", string(certs.CA)),
		"--host-management-prefix", managementPrefix, "--host-egress-interface", "eth0", "--host-tunnel-port", "51820", "--out", profile), "gen host-profile")
	f.must(f.orch("host", "profile", "set", "-f", profile), "host profile set")

	const total = 2
	f.serveDHCP(total, 2)
	nd = f.bootFromSeed(iso, 2, total)
	p := f.pendingWith(node.QEMUNetwork(2, total).MAC)
	id = f.admit(p.Code, node.HostRegion)
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, joinApplied), "the host applied its configuration")
	awaitRestarting(t, nd, "orch hears from the host", func() bool {
		for _, h := range f.hostList(1) {
			if h.ID == id {
				return !h.LastSeen.IsZero()
			}
		}
		return false
	})
	return f, nd, id
}

// orchHostClient is a host API client at addr that presents orch's certificate, which the host trusts and
// which holds the provisioner role (ADR 0017), and that checks nothing of the host's certificate.
func (f *fleet) orchHostClient(t *testing.T, addr string) *hostapi.Client {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(filepath.Join(f.dir, orchName(1)+".crt"), f.keys[1])
	require.NoError(t, err)
	return hostapi.NewClient("https://"+addr, &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}})
}

func TestFleet_AWipeOfTheDataDiskMakesTheHostFormatItAgainAtItsNextBoot(t *testing.T) {
	f, nd, id := joinedHost(t)
	formatted := func() int { return strings.Count(nd.Console(), "data disk formatted as ext4") }
	require.Equal(t, 1, formatted(), "the control: the blank data disk was formatted at the first boot")
	api := nodeIP(2) + ":7443"

	_, err := f.orchHostClient(t, api).Reset(t.Context(), hostapi.ResetRequest{Scope: hostapi.ResetAll, Confirm: id})
	require.NoError(t, err)
	select {
	case <-nd.Done():
	case <-time.After(bootTimeout):
		t.Fatal("the host did not reboot")
	}
	require.NoError(t, nd.Reboot())

	// The console is the new boot's: the disk it finds is blank, so init formats it as it did on the first boot.
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "resetting as the host asked", "scope=all"))
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "data disk formatted as ext4"))
	assert.Equal(t, 1, formatted(), "this boot formatted the disk")
	eventually(t, "the host enrolls again from its seed, as a new host", func() bool { return len(f.pending()) > 0 })
}

func TestFleet_ARefusedHostIsReachableInMaintenanceByACertificateUnderOrchsAuthorityAndRefusedOnceItIsRevoked(t *testing.T) {
	f, nd, id := joinedHost(t)
	api := nodeIP(2) + ":7443"
	_, err := f.orchHostClient(t, api).Maintenance(t.Context())
	require.Error(t, err, "the control: a host that booted is not in maintenance, and answers 404")
	var apiErr *hostapi.Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.Status)

	// The operator breaks the seed and restarts the host: the boot is refused. The seed names no
	// orch, so maintenance takes the anchor the last good boot stored.
	require.NoError(t, nd.ReplaceUserData([]byte("spec: [this is not a NodeConfig"), []byte("version: 2\n")))
	require.NoError(t, nd.Reboot())
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "boot refused"))

	var m hostapi.Maintenance
	eventually(t, "a certificate of orch's authority reads the maintenance report", func() bool {
		var err error
		m, err = f.orchHostClient(t, api).Maintenance(t.Context())
		return err == nil
	})
	assert.Equal(t, id, m.HostID)
	assert.NotEmpty(t, m.Reason)

	// Root revokes the certificate: maintenance refuses it once its list reaches the host.
	serial := hostCertSerial(t, f.forward(1, orchPort))
	f.must(nrun("operator", "revoke", "--admin", f.root.file, "--passphrase-file", f.pass, "--serial", serial, "--config", f.c.API.Admin), "revoke orch's certificate")
	eventually(t, "maintenance refuses the revoked certificate", func() bool {
		_, err := f.orchHostClient(t, api).Maintenance(t.Context())
		return err != nil
	})
}
