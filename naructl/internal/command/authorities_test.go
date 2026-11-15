package command_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// authorityCert is a self-signed authority certificate named cn.
func authorityCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func TestSignerCA_PrintsEachAuthorityAsPEMWithTheHashOfItsDER(t *testing.T) {
	signer, node, config := signerThroughNode(t)
	orch, operator, host := authorityCert(t, "orch authority"), authorityCert(t, "operator authority"), authorityCert(t, "host authority")
	signer.authorities = identity.Authorities{Orch: orch.Raw, Operator: operator.Raw, Host: host.Raw}

	text, err := run(t, "signer", "ca", "--config", config, "--node", node.addr())
	require.NoError(t, err, text)
	assert.Contains(t, node.requests, "GET /v1/signer/ca", "through the Node API's route to the signer")
	assert.Equal(t,
		"# orch authority, SHA-256 "+fingerprint(orch)+"\n"+pemOf(orch)+
			"# operator authority, SHA-256 "+fingerprint(operator)+"\n"+pemOf(operator)+
			"# host authority, SHA-256 "+fingerprint(host)+"\n"+pemOf(host), text)

	text, err = run(t, "signer", "ca", "--authority", "host", "--socket", signer.socket)
	require.NoError(t, err, text)
	assert.Equal(t, "# host authority, SHA-256 "+fingerprint(host)+"\n"+pemOf(host), text, "one authority, reached on the socket")

	text, err = run(t, "signer", "ca", "--authority", "root", "--socket", signer.socket)
	require.Error(t, err)
	assert.Contains(t, text, "--authority")

	text, err = run(t, "signer", "ca")
	require.Error(t, err)
	assert.Contains(t, text, "--socket or --config is required")
}

func TestOrchCA_PrintsTheOrchAuthorityAlone(t *testing.T) {
	signer, _, config := signerThroughNode(t)
	orch, operator, host := authorityCert(t, "orch authority"), authorityCert(t, "operator authority"), authorityCert(t, "host authority")
	signer.authorities = identity.Authorities{Orch: orch.Raw, Operator: operator.Raw, Host: host.Raw}

	text, err := run(t, "orch", "ca", "--config", config)
	require.NoError(t, err, text)
	assert.Equal(t, "# orch authority, SHA-256 "+fingerprint(orch)+"\n"+pemOf(orch), text)
}

func TestHostList_ShowsEachHostWithItsMachinePrefixAndWhenItWasLastSeen(t *testing.T) {
	seen := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)
	f := newFakeOrch(t)
	f.hosts = []orchapi.Host{
		{ID: "h-0123456789abcdef", Region: "kr-seoul", Address: "10.78.0.3:7443", Labels: map[string]string{"rack": "a", "disk": "nvme"}, MachinePrefix: "10.77.1.0/24", LastSeen: seen},
		{ID: "naru-node-1", Region: "kr-seoul", Address: "10.78.0.1:7443"},
	}
	op := newOperator(t, f, "operator")

	text, err := run(t, op.args(f, "host", "list")...)
	require.NoError(t, err, text)
	assert.Regexp(t, `HOST\s+REGION\s+ADDRESS\s+MACHINE PREFIX\s+LAST SEEN\s+LABELS`, text)
	assert.Regexp(t, `h-0123456789abcdef\s+kr-seoul\s+10\.78\.0\.3:7443\s+10\.77\.1\.0/24\s+2026-11-15T01:02:03Z\s+disk=nvme,rack=a`, text)
	assert.Regexp(t, `naru-node-1\s+kr-seoul\s+10\.78\.0\.1:7443\s+-\s+never\s+-`, text)
}
