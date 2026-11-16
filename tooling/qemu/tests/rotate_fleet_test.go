//go:build qemu_real

package tests_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// hostPairOnDisk copies the pair a host enrolled with from its data disk into dir, as files only this
// user reads, and returns their paths. The pair's key is never printed.
func hostPairOnDisk(t *testing.T, nd *node.Node, dir, name string) (crt, key string) {
	t.Helper()
	crt, key = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	eventually(t, nd.Name+"'s data disk holds the pair it enrolled with", func() bool {
		sn, err := nd.SnapshotData(filepath.Join(t.TempDir(), "data.img"))
		if err != nil {
			return false
		}
		c, err := sn.Read(wire.DataMount + "/hostd/host.crt")
		if err != nil {
			return false
		}
		k, err := sn.Read(wire.DataMount + "/hostd/host.key")
		if err != nil {
			return false
		}
		return os.WriteFile(crt, c, 0o600) == nil && os.WriteFile(key, k, 0o600) == nil
	})
	return crt, key
}

// serialOfFile is the serial, in hex, of the first certificate of a PEM file.
func serialOfFile(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block, file)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert.SerialNumber.Text(16)
}

// heartbeatStatus is the status orch at addr answers a heartbeat from the pair in the two files, or the
// error of a handshake orch refused.
func heartbeatStatus(addr, certFile, keyFile string) (int, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return 0, err
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}}
	resp, err := c.Post("https://"+addr+"/v1/heartbeat", "application/json", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// servedCertificate is the certificate naru-hostd at addr presents, as PEM in a file of dir, and its CN.
func servedCertificate(t *testing.T, addr, dir, name string) (file, cn string) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	cert := conn.ConnectionState().PeerCertificates[0]
	file = filepath.Join(dir, name+".crt")
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	return file, cert.Subject.CommonName
}

// Rotating a running host (ADR 0077): the host takes the new pair first, uses it with orch, and only then
// does the signer revoke what it held, so orch hears the host throughout; a client presenting the old pair
// is refused once the revocation list reaches orch.
func TestFleet_RotatingARunningHostDeliversTheNewPairBeforeTheOldIsRevoked(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h)
	hosts := f.hostList(1)
	require.Len(t, hosts, 1)
	id := hosts[0].ID
	lastSeen := func() time.Time {
		if hs := f.hostList(1); len(hs) > 0 {
			return hs[0].LastSeen
		}
		return time.Time{}
	}
	eventually(t, "orch hears from the host", func() bool { return !lastSeen().IsZero() })

	oldCrt, oldKey := hostPairOnDisk(t, f.node(h), f.dir, "old")
	oldSerial := serialOfFile(t, oldCrt)
	orchAddr := f.forward(1, orchPort)
	code, err := heartbeatStatus(orchAddr, oldCrt, oldKey)
	require.NoError(t, err, "orch takes the pair the host enrolled with before the rotation")
	require.Equal(t, http.StatusNoContent, code)

	before := lastSeen()
	out := f.must(nrun("signer", "rotate-host", "--admin", f.root.file, "--passphrase-file", f.pass, "--host-id", id,
		"--host-client", api.Provisioner, "--config", f.c.API.Admin), "rotate the running host's pair")

	// The host holds and uses the new pair: its data disk has it, and orch hears the host again.
	newCrt, _ := hostPairOnDisk(t, f.node(h), f.dir, "new")
	newSerial := serialOfFile(t, newCrt)
	assert.NotEqual(t, oldSerial, newSerial, "the host took a new pair")
	assert.Contains(t, out, newSerial, "naructl reports the serial the host reports with")
	eventually(t, "orch hears the host again with the new pair", func() bool { return lastSeen().After(before) })

	// The old serial is revoked: orch refuses a client that presents the old pair.
	eventually(t, "orch refuses the old pair", func() bool {
		code, err := heartbeatStatus(orchAddr, oldCrt, oldKey)
		return err != nil || code != http.StatusNoContent
	})
	// The host stays up, and its own API still takes the operator's client.
	r := hctl(api.Provisioner, []string{"microvm", "list"})
	require.NoError(t, r.Err, r.Stderr)
}

// A host whose configuration carries a revoked key refuses it (ADR 0077): the pair gen node wrote is
// registered by its serial, the rotation revokes it, and naru-hostd stops serving with it, while it runs
// and again after it restarts from the configuration that still carries it.
func TestFleet_RotatingAHostRevokesAnOfflinePairRegisteredBySerialAndTheHostRefusesItAfterARestart(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h)
	hosts := f.hostList(1)
	require.Len(t, hosts, 1)
	id := hosts[0].ID

	servingCrt, cn := servedCertificate(t, api.Addr, f.dir, "serving")
	require.Equal(t, id, cn, "the pair gen node wrote names the host orch knows")
	f.must(nrun("signer", "register-host", "--admin", f.root.file, "--passphrase-file", f.pass, "--host-id", id,
		"--cert", servingCrt, "--config", f.c.API.Admin), "register the offline pair by its serial")

	f.must(nrun("signer", "rotate-host", "--admin", f.root.file, "--passphrase-file", f.pass, "--host-id", id,
		"--host-client", api.Provisioner, "--config", f.c.API.Admin), "rotate the host's pair")

	refuses := func() bool {
		conn, err := net.DialTimeout("tcp", api.Addr, 5*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}
	eventually(t, "the host stops serving with the revoked pair of its configuration", refuses)

	// Restarted from the same configuration, which still carries the revoked pair, it refuses at start.
	const refusal = "refusing to serve with a revoked key"
	was := strings.Count(f.node(h).Console(), refusal)
	f.edit(h, func(string) error { return nil })
	eventually(t, "the restarted host says it refuses a revoked key", func() bool {
		return strings.Count(f.node(h).Console(), refusal) > was
	})
	assert.True(t, refuses(), "the restarted host does not serve with the revoked pair")
}
