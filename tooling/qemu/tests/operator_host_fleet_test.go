//go:build qemu_real

package tests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// An operator the signer issued a certificate to reads and provisions a host that joined by enrollment,
// with that certificate and no client file of the host authority (ADR 0080): the host trusts the
// operator authority orch's callers are checked against, and takes the role from the certificate.
func TestFleet_AnOperatorTheSignerIssuedACertificateToCallsAnEnrolledHostsAPIWithItsRole(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h).Addr
	operator, reader := f.operator("ops-1", "operator"), f.operator("ops-2", "reader")

	call := func(p person, words ...string) result {
		args := append(append([]string{}, words...), "--identity", p.file, "--passphrase-file", f.pass, "--host-ca", f.c.HostCA, "--endpoint", api)
		return nrun(args...)
	}
	out := f.must(call(operator, "host"), "an operator reads the host")
	assert.Contains(t, out, `"region": "eu-1"`)
	f.must(call(reader, "host"), "a reader reads the host")
	f.must(call(operator, "microvm", "list"), "an operator lists microVMs")

	// A reader may not write: the refusal is the role's, not the handshake's, since the same
	// certificate read a moment ago.
	r := call(reader, "microvm", "delete", "absent")
	require.Error(t, r.Err)
	assert.Contains(t, r.Stderr, "403", "a reader's delete is refused by role")
	// An operator's delete reaches the handler, which does not find the microVM.
	r = call(operator, "microvm", "delete", "absent")
	require.Error(t, r.Err)
	assert.Contains(t, r.Stderr, "404", "an operator's delete is allowed, and finds no such microVM")
}

// An operator the signer issued a certificate to adds a boot set to a host that joined by
// enrollment, and the host lists it (#260): the add that `naructl host boot-set add` makes without
// the host authority's client file is the operator's own, and a reader's is refused by role.
func TestFleet_AnOperatorTheSignerIssuedACertificateToAddsABootSetToAnEnrolledHostAndTheHostListsIt(t *testing.T) {
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")
	if image == "" || guestDir == "" {
		t.Skip("NARU_IMAGE_DIR and NARU_GUEST_DIR name the files make image wrote")
	}
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h).Addr
	operator, reader := f.operator("ops-1", "operator"), f.operator("ops-2", "reader")

	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-operator",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), filepath.Join(image, "node-initrd.cpio.gz")))
	const set = "naru-node-qemu-operator"
	manifest := filepath.Join(assets, set+".json")
	call := func(p person, words ...string) result {
		args := append(append([]string{}, words...), "--identity", p.file, "--passphrase-file", f.pass, "--host-ca", f.c.HostCA, "--endpoint", api)
		return nrun(args...)
	}
	add := func(p person) result { return call(p, "host", "boot-set", "add", manifest) }

	r := add(reader)
	require.Error(t, r.Err)
	assert.Contains(t, r.Stderr, "403", "a reader's add is refused by role")
	assert.NotContains(t, f.must(call(reader, "host"), "a reader reads the host"), set)

	added := f.must(add(operator), "an operator adds the boot set")
	assert.Contains(t, added, "added boot set "+set)
	eventually(t, "the host lists the added boot set", func() bool {
		r := call(operator, "host")
		return r.Err == nil && strings.Contains(r.Stdout, set)
	})
}
