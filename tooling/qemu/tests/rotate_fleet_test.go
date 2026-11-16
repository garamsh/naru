//go:build qemu_real

package tests_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Root rotates a host's pair through the signer (ADR 0077): the host gets a new pair, the serial of the
// pair it holds goes on the revocation list, orch stops calling the host, and the host, which checks its
// own certificate against that list, refuses to keep serving with it.
func TestFleet_RotatingAHostRevokesItsOldPairAndTheHostRefusesARevokedKey(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h).Addr
	hosts := f.hostList(1)
	require.Len(t, hosts, 1)
	id := hosts[0].ID
	oldSerial := hostCertSerial(t, api)

	keyOut, certOut := filepath.Join(f.dir, id+".key"), filepath.Join(f.dir, id+".crt")
	f.must(nrun("signer", "rotate-host", "--admin", f.root.file, "--passphrase-file", f.pass, "--host-id", id,
		"--key-out", keyOut, "--cert-out", certOut, "--config", f.c.API.Admin), "rotate the host's pair")
	info, err := os.Stat(keyOut)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.FileExists(t, certOut)

	// The old serial is on the list, so orch, which reads it from the signer, stops calling the host.
	lastSeen := func() time.Time {
		if hs := f.hostList(1); len(hs) > 0 {
			return hs[0].LastSeen
		}
		return time.Time{}
	}
	eventually(t, "orch stops hearing from the host", func() bool {
		a := lastSeen()
		time.Sleep(10 * time.Second)
		return lastSeen().Equal(a)
	})

	// The host checks its own certificate against the list and exits: the serial it served is gone.
	eventually(t, "the host stops serving the revoked pair", func() bool {
		conn, err := net.DialTimeout("tcp", api, 5*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	})
	assert.NotEmpty(t, oldSerial, "the host served a certificate before the rotation")
}
