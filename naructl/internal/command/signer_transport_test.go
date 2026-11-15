package command_test

import (
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// signerThroughNode is a fake signer on a socket and a fake Node whose API forwards /v1/signer to it.
func signerThroughNode(t *testing.T) (*fakeSigner, *fakeNode, string) {
	t.Helper()
	dep := newDeployment(t)
	apiAuthority(t, dep)
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	signer := startFakeSigner(t, pool)
	node := newFakeNode(t, dep)
	node.signerSocket = signer.socket
	return signer, node, node.clientFile(t, dep, "admin")
}

func TestSignerCommands_ThroughANodesAPIReachTheSignerAtItsOwnPath(t *testing.T) {
	signer, node, config := signerThroughNode(t)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	csr, _ := csrFile(t)
	out := filepath.Join(t.TempDir(), "alice.crt")

	text, err := run(t, "operator", "add", "--admin", admin, "--passphrase-file", pass, "--csr", csr,
		"--name", "alice", "--role", "reader", "--config", config, "--endpoint", node.addr(), "--out", out)
	require.NoError(t, err, text)

	assert.Contains(t, node.requests, "POST /v1/signer/operator", "the request went to the Node API")
	assert.Equal(t, identity.RoleRoot, signer.who["/v1/operator"].Role, "and reached the signer at /v1/operator with the admin file's signature")
	_, err = os.Stat(out)
	require.NoError(t, err, "the certificate the signer issued came back")

	text, err = run(t, "operator", "revoke", "--admin", admin, "--passphrase-file", pass, "--serial", "ABCD", "--config", config)
	require.NoError(t, err, text)
	var req identity.RevokeRequest
	require.NoError(t, json.Unmarshal(signer.got["/v1/revoke"], &req))
	assert.Equal(t, "abcd", req.Serial)
}

func TestSignerCommands_AReaderClientFileIsRefusedByTheNodeAndTheSignerHearsNothing(t *testing.T) {
	signer, _, _ := signerThroughNode(t)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")

	// The fake Node lets a reader GET and refuses anything else; revoke is a POST.
	dep := newDeployment(t)
	apiAuthority(t, dep)
	other := newFakeNode(t, dep)
	other.signerSocket = signer.socket
	reader := other.clientFile(t, dep, "reader")

	text, err := run(t, "operator", "revoke", "--admin", admin, "--passphrase-file", pass, "--serial", "AB", "--config", reader)
	require.Error(t, err)
	assert.Contains(t, text, "refused")
	assert.Contains(t, text, "403")
	assert.Empty(t, signer.got, "nothing reached the signer")
}

func TestSignerCommands_TakeExactlyOneOfSocketAndConfigAndEndpointOnlyWithConfig(t *testing.T) {
	signer, _, config := signerThroughNode(t)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	base := []string{"operator", "revoke", "--admin", admin, "--passphrase-file", pass, "--serial", "AB"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	text, err := run(t, with("--socket", signer.socket, "--config", config)...)
	require.Error(t, err)
	assert.Contains(t, text, "--socket and --config")

	text, err = run(t, with()...)
	require.Error(t, err)
	assert.Contains(t, text, "--socket or --config is required")

	text, err = run(t, with("--socket", signer.socket, "--endpoint", "10.0.0.1")...)
	require.Error(t, err)
	assert.Contains(t, text, "--endpoint")
	assert.Empty(t, signer.got, "every refusal came before a request")

	text, err = run(t, with("--socket", signer.socket)...)
	require.NoError(t, err, text)
	assert.NotEmpty(t, signer.got)
}

func TestSignerCommands_EachOfTheSixTakesConfig(t *testing.T) {
	_, _, config := signerThroughNode(t)
	for _, args := range [][]string{
		{"operator", "add"}, {"operator", "revoke"}, {"signer", "bundle"}, {"signer", "restore"}, {"signer", "reissue-root"}, {"orch", "issue"},
	} {
		t.Run(args[0]+" "+args[1], func(t *testing.T) {
			text, err := run(t, append(args, "--config", config, "--socket", "x")...)
			require.Error(t, err)
			assert.Contains(t, text, "--socket and --config", "the flag is known, and the pair is refused")
		})
	}
}
