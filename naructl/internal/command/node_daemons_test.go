package command_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// daemonInputs are the files the flags of gen node name, made from throwaway keys.
type daemonInputs struct {
	dir                                                                  string
	cert, pub, csr, key, recovery, enroll, authorisation                 string
	certPEM, keyPEM, enrollKey, authorisationText, recoveryText, csrText string
}

func newDaemonInputs(t *testing.T) daemonInputs {
	t.Helper()
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "orch"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "root"}}, priv)
	require.NoError(t, err)
	enc := func(typ string, b []byte) string { return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})) }
	in := daemonInputs{
		dir: dir, certPEM: enc("CERTIFICATE", certDER), keyPEM: enc("PRIVATE KEY", keyDER), enrollKey: "enrollment-key-value\n",
		authorisationText: `{"signed":"authorisation"}`, recoveryText: `{"x25519":"AA","ed25519":"AA"}`, csrText: enc("CERTIFICATE REQUEST", csrDER),
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	in.cert, in.pub, in.csr, in.key = write("orch.crt", in.certPEM), write("revocation.pub", enc("PUBLIC KEY", pubDER)), write("root.csr", in.csrText), write("orch.key", in.keyPEM)
	in.recovery, in.enroll, in.authorisation = write("recovery.json", in.recoveryText), write("enroll.key", in.enrollKey), write("authorisation", in.authorisationText)
	return in
}

// hostNode makes the host authority and returns the flags of a Node that serves the host API.
func (dep deployment) hostFlags(t *testing.T) []string {
	t.Helper()
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	return []string{"--host-region", "seoul"}
}

type daemonsData struct {
	Spec struct {
		Host struct {
			Network *struct {
				ManagementPrefix string `yaml:"managementPrefix"`
				EgressInterface  string `yaml:"egressInterface"`
				TunnelPort       int    `yaml:"tunnelPort"`
			} `yaml:"network"`
			Orch *struct {
				URL       string `yaml:"url"`
				CASHA256  string `yaml:"caSHA256"`
				EnrollKey string `yaml:"enrollKey"`
			} `yaml:"orch"`
		} `yaml:"host"`
		Signer *struct {
			PeerListen string   `yaml:"peerListen"`
			PeerNames  []string `yaml:"peerNames"`
			Bootstrap  *struct {
				RecoveryPublicKey string `yaml:"recoveryPublicKey"`
				RootCSR           string `yaml:"rootCSR"`
			} `yaml:"bootstrap"`
			Replica *struct {
				Join *struct {
					Peer          string `yaml:"peer"`
					PeerCA        string `yaml:"peerCA"`
					Authorisation string `yaml:"authorisation"`
				} `yaml:"join"`
			} `yaml:"replica"`
		} `yaml:"signer"`
		Orch *struct {
			PeerURL             string            `yaml:"peerURL"`
			ClientURL           string            `yaml:"clientURL"`
			InitialCluster      map[string]string `yaml:"initialCluster"`
			InitialClusterState string            `yaml:"initialClusterState"`
			Listen              string            `yaml:"listen"`
			SANs                []string          `yaml:"sans"`
			Certificate         string            `yaml:"certificate"`
			CA                  string            `yaml:"ca"`
			RevocationPublicKey string            `yaml:"revocationPublicKey"`
			RecoveryPublicKey   string            `yaml:"recoveryPublicKey"`
			Restore             bool              `yaml:"restore"`
		} `yaml:"orch"`
		Metrics *struct {
			Listen string `yaml:"listen"`
		} `yaml:"metrics"`
		Files []struct {
			Path    string `yaml:"path"`
			Mode    string `yaml:"mode"`
			Content string `yaml:"content"`
			Owner   *struct {
				UID uint32 `yaml:"uid"`
				GID uint32 `yaml:"gid"`
			} `yaml:"owner"`
		} `yaml:"files"`
	} `yaml:"spec"`
}

func parseDaemons(t *testing.T, path string) daemonsData {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var d daemonsData
	require.NoError(t, yaml.Unmarshal(b, &d))
	return d
}

func TestGenNode_WritesTheDaemonSectionsAndInitAcceptsThem(t *testing.T) {
	dep := newDeployment(t)
	in := newDaemonInputs(t)
	flags := append(dep.hostFlags(t),
		"--host-management-prefix", "192.168.0.0/24", "--host-egress-interface", "eth0", "--host-tunnel-port", "51821",
		"--host-orch-url", "https://192.168.0.10:7500", "--host-orch-ca-sha256", d("a"), "--host-orch-enroll-key", in.enroll,
		"--signer", "--signer-peer-listen", "192.168.0.10:7600", "--signer-peer-name", "192.168.0.10",
		"--signer-recovery-public", in.recovery, "--signer-root-csr", in.csr,
		"--orch-peer-url", "https://192.168.0.10:2380", "--orch-client-url", "https://192.168.0.10:2379",
		"--orch-member", "naru-node-1=https://192.168.0.10:2380", "--orch-member", "node-2=https://192.168.0.20:2380",
		"--orch-san", "192.168.0.10", "--orch-san", "127.0.0.1", "--orch-state", "new", "--orch-listen", "0.0.0.0:7500",
		"--orch-certificate", in.cert, "--orch-ca", in.cert, "--orch-revocation-public-key", in.pub, "--orch-recovery-public", in.recovery, "--orch-key", in.key, "--orch-restore",
		"--metrics-listen", "192.168.0.10",
	)

	path := dep.node(t, "node-1", flags...)

	validate(t, path)
	got := parseDaemons(t, path)
	require.NotNil(t, got.Spec.Host.Network)
	assert.Equal(t, "192.168.0.0/24", got.Spec.Host.Network.ManagementPrefix)
	assert.Equal(t, "eth0", got.Spec.Host.Network.EgressInterface)
	assert.Equal(t, 51821, got.Spec.Host.Network.TunnelPort)
	require.NotNil(t, got.Spec.Host.Orch)
	assert.Equal(t, "https://192.168.0.10:7500", got.Spec.Host.Orch.URL)
	assert.Equal(t, d("a"), got.Spec.Host.Orch.CASHA256)
	assert.Equal(t, in.enrollKey, got.Spec.Host.Orch.EnrollKey)
	require.NotNil(t, got.Spec.Signer)
	assert.Equal(t, "192.168.0.10:7600", got.Spec.Signer.PeerListen)
	assert.Equal(t, []string{"192.168.0.10"}, got.Spec.Signer.PeerNames)
	require.NotNil(t, got.Spec.Signer.Bootstrap)
	assert.Equal(t, in.recoveryText, got.Spec.Signer.Bootstrap.RecoveryPublicKey)
	assert.Equal(t, in.csrText, got.Spec.Signer.Bootstrap.RootCSR)
	assert.Nil(t, got.Spec.Signer.Replica)
	require.NotNil(t, got.Spec.Orch)
	assert.Equal(t, map[string]string{"naru-node-1": "https://192.168.0.10:2380", "node-2": "https://192.168.0.20:2380"}, got.Spec.Orch.InitialCluster)
	assert.Equal(t, "https://192.168.0.10:2379", got.Spec.Orch.ClientURL)
	assert.Equal(t, []string{"192.168.0.10", "127.0.0.1"}, got.Spec.Orch.SANs)
	assert.Equal(t, in.certPEM, got.Spec.Orch.Certificate)
	assert.Equal(t, in.recoveryText, got.Spec.Orch.RecoveryPublicKey)
	assert.True(t, got.Spec.Orch.Restore)
	require.NotNil(t, got.Spec.Metrics)
	assert.Equal(t, "192.168.0.10", got.Spec.Metrics.Listen)

	var found bool
	for _, f := range got.Spec.Files {
		if f.Path != "/run/naru-orchd/orch.key" {
			continue
		}
		found = true
		assert.Equal(t, in.keyPEM, f.Content)
		assert.Equal(t, "0600", f.Mode, "orch's private key is readable by its owner alone")
		require.NotNil(t, f.Owner)
		assert.Equal(t, uint32(6002), f.Owner.UID, "orch runs as 6002 and must read it")
		assert.Equal(t, uint32(6002), f.Owner.GID)
	}
	assert.True(t, found, "orch's key is a spec.files entry, which GET /v1/config redacts")
}

func TestGenNode_ASignerReplicaMakesItsJoinKeyFirstAndJoinsOnceAuthorised(t *testing.T) {
	dep := newDeployment(t)
	in := newDaemonInputs(t)

	first := parseDaemons(t, dep.node(t, "node-1", "--signer", "--signer-replica", "--signer-peer-listen", "192.168.0.10:7600", "--signer-peer-name", "192.168.0.10"))
	require.NotNil(t, first.Spec.Signer)
	require.NotNil(t, first.Spec.Signer.Replica, "a replica: {} makes the join key at the first start")
	assert.Nil(t, first.Spec.Signer.Replica.Join)
	assert.Nil(t, first.Spec.Signer.Bootstrap)

	path := dep.node(t, "node-1", "--signer", "--signer-replica", "--signer-peer-listen", "192.168.0.10:7600", "--signer-peer-name", "192.168.0.10",
		"--signer-join-peer", "192.168.0.20:7600", "--signer-join-peer-ca", in.cert, "--signer-join-authorisation", in.authorisation)
	validate(t, path)
	second := parseDaemons(t, path)
	require.NotNil(t, second.Spec.Signer.Replica)
	require.NotNil(t, second.Spec.Signer.Replica.Join)
	assert.Equal(t, "192.168.0.20:7600", second.Spec.Signer.Replica.Join.Peer)
	assert.Equal(t, in.certPEM, second.Spec.Signer.Replica.Join.PeerCA)
	assert.Equal(t, in.authorisationText, second.Spec.Signer.Replica.Join.Authorisation)
}

func TestGenNode_NoDaemonFlagsLeavesTheSectionsOut(t *testing.T) {
	dep := newDeployment(t)

	got := parseDaemons(t, dep.node(t, "node-1"))

	assert.Nil(t, got.Spec.Signer)
	assert.Nil(t, got.Spec.Orch)
	assert.Nil(t, got.Spec.Metrics)
	assert.Nil(t, got.Spec.Host.Network)
	assert.Nil(t, got.Spec.Host.Orch)
}

func TestGenNode_DaemonFlagsRefuseWhatInitWouldRefuseOrWhatDoesNothing(t *testing.T) {
	dep := newDeployment(t)
	in := newDaemonInputs(t)
	host := dep.hostFlags(t)
	base := []string{
		"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"),
	}
	orch := []string{
		"--signer", "--orch-peer-url", "https://192.168.0.10:2380", "--orch-client-url", "https://192.168.0.10:2379",
		"--orch-member", "naru-node-1=https://192.168.0.10:2380", "--orch-certificate", in.cert, "--orch-ca", in.cert,
		"--orch-revocation-public-key", in.pub, "--orch-key", in.key,
	}
	without := func(drop string) []string {
		var out []string
		for i := 0; i < len(orch); i++ {
			if orch[i] == drop {
				if drop != "--signer" {
					i++
				}
				continue
			}
			out = append(out, orch[i])
		}
		return out
	}
	tests := []struct {
		name  string
		flags []string
		want  string
	}{
		{"host network without the host API", []string{"--host-management-prefix", "192.168.0.0/24"}, "needs the host API"},
		{"management prefix with host bits", append([]string{"--host-management-prefix", "192.168.0.1/24"}, host...), "no host bits"},
		{"egress without a management prefix", append([]string{"--host-egress-interface", "eth0"}, host...), "--host-egress-interface has no effect without --host-management-prefix"},
		{"host orch without the host API", []string{"--host-orch-url", "https://192.168.0.10:7500", "--host-orch-ca-sha256", d("a"), "--host-orch-enroll-key", in.enroll}, "needs the host API"},
		{"host orch without a key", append([]string{"--host-orch-url", "https://192.168.0.10:7500", "--host-orch-ca-sha256", d("a")}, host...), "--host-orch-enroll-key"},
		{"orch without the signer", without("--signer"), "run the signer here"},
		{"orch without its key", without("--orch-key"), "needs its certificate"},
		{"orch cluster without this member", append(without("--orch-member"), "--orch-member", "node-2=https://192.168.0.20:2380"), "initial cluster holds"},
		{"orch flag without --orch-peer-url", []string{"--orch-ca", in.cert}, "--orch-ca has no effect without --orch-peer-url"},
		{"bootstrap beside replica", []string{"--signer", "--signer-replica", "--signer-recovery-public", in.recovery, "--signer-root-csr", in.csr}, "not both"},
		{"bootstrap with one half", []string{"--signer", "--signer-root-csr", in.csr}, "both the recovery public key and the root request"},
		{"join without replica", []string{"--signer", "--signer-join-peer", "192.168.0.20:7600"}, "--signer-join-peer has no effect without --signer-replica"},
		{"signer flag without --signer", []string{"--signer-peer-listen", "192.168.0.10:7600"}, "--signer-peer-listen has no effect without --signer"},
		{"metrics listen not an address", []string{"--metrics-listen", "example.com"}, "not an IP address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(append([]string{}, base...), "--out", filepath.Join(t.TempDir(), "x"))
			out, err := run(t, append(args, tt.flags...)...)
			require.Error(t, err, out)
			assert.Contains(t, out, tt.want)
		})
	}
}

func TestGenNode_TheOrchKeyAndTheEnrollmentKeyAreNeverPrinted(t *testing.T) {
	dep := newDeployment(t)
	in := newDaemonInputs(t)
	out := filepath.Join(t.TempDir(), "x")
	args := []string{
		"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out,
		"--signer", "--orch-peer-url", "https://192.168.0.10:2380", "--orch-client-url", "https://192.168.0.10:2379",
		"--orch-member", "naru-node-1=https://192.168.0.10:2380", "--orch-certificate", in.cert, "--orch-ca", in.cert,
		"--orch-revocation-public-key", in.pub, "--orch-key", in.key,
	}

	printed, err := run(t, append(args, dep.hostFlags(t)...)...)

	require.NoError(t, err, printed)
	assert.NotContains(t, printed, "PRIVATE KEY")
	assert.NotContains(t, printed, in.keyPEM)
	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the user data carries keys")
}
