package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/config"
)

// throwaway returns the PEM of a self-signed certificate, of a PKIX public key and of a
// certificate request, each from a key made for the test.
func throwaway(t *testing.T) (cert, pub, csr string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}}, &key.PublicKey, key)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "root"}}, key)
	require.NoError(t, err)
	enc := func(typ string, b []byte) string { return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})) }
	return enc("CERTIFICATE", der), enc("PUBLIC KEY", pubDER), enc("CERTIFICATE REQUEST", csrDER)
}

func indent(s string) string {
	return "        " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n        ") + "\n"
}

// orchDigest is the SHA-256 daemonsDoc gives its naru-orch extension.
const orchDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// daemonsDoc is a Node that runs hostd, the signer and orch, with every section set.
func daemonsDoc(t *testing.T) string {
	cert, pub, csr := throwaway(t)
	return `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: host-1
spec:
  services:
    - name: naru-hostd
      binary: /bin/naru-hostd
      args: ["--listen=0.0.0.0:7443"]
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  extensions:
    - name: naru-orch
      sha256: ` + orchDigest + `
  host:
    region: seoul
    network:
      managementPrefix: 192.168.0.0/24
      egressInterface: eth0
      tunnelPort: 51821
    orch:
      url: https://192.168.0.40:7500
      caSHA256: abababababababababababababababababababababababababababababababab
      enrollKey: enroll-secret
  signer:
    peerListen: 192.168.0.40:7600
    peerNames: [192.168.0.40]
    bootstrap:
      recoveryPublicKey: '{"x25519":"AA","ed25519":"AA"}'
      rootCSR: |
` + indent(csr) + `  orch:
    peerURL: https://192.168.0.40:2380
    clientURL: https://192.168.0.40:2379
    initialCluster:
      host-1: https://192.168.0.40:2380
      host-2: https://192.168.0.41:2380
    sans: [192.168.0.40, 127.0.0.1]
    certificate: |
` + indent(cert) + `    ca: |
` + indent(cert) + `    hostCA: |
` + indent(cert) + `    revocationPublicKey: |
` + indent(pub) + `    recoveryPublicKey: '{"x25519":"AA","ed25519":"AA"}'
  metrics:
    listen: 192.168.0.40
  files:
    - path: /run/naru-orchd/orch.key
      mode: "0600"
      content: not-a-real-key
      owner: {uid: 6002, gid: 6002}
`
}

func service(t *testing.T, cfg config.NodeConfig, name string) config.Service {
	t.Helper()
	for _, s := range cfg.Spec.Services {
		if s.Name == name {
			return s
		}
	}
	require.Failf(t, "no such service", "no service named %s in %v", name, cfg.Spec.Services)
	return config.Service{}
}

func file(t *testing.T, cfg config.NodeConfig, path string) config.File {
	t.Helper()
	for _, f := range cfg.Spec.Files {
		if f.Path == path {
			return f
		}
	}
	require.Failf(t, "no such file", "no file at %s", path)
	return config.File{}
}

func TestParse_SectionsBecomeServicesAndFlags(t *testing.T) {
	cfg, err := config.Parse([]byte(daemonsDoc(t)))
	require.NoError(t, err)

	hostd := service(t, cfg, "naru-hostd")
	assert.Equal(t, []string{
		"--listen=0.0.0.0:7443", "--management-prefix=192.168.0.0/24",
		"--tunnel-port=51821", "--egress-interface=eth0",
		"--orch-url=https://192.168.0.40:7500", "--orch-ca-sha256=abababababababababababababababababababababababababababababababab", "--enroll-key-file=/run/naru-hostd/enroll.key",
		"--metrics-addr=192.168.0.40:9101",
	}, hostd.Args)
	enroll := file(t, cfg, "/run/naru-hostd/enroll.key")
	assert.Equal(t, "enroll-secret", enroll.Content)
	assert.Equal(t, "0600", enroll.Mode, "the enrollment key is hostd's alone")
	assert.Nil(t, enroll.Owner, "hostd runs as root")
	assert.Equal(t, []string{"naru-hostd"}, enroll.Restart)

	signer := service(t, cfg, "naru-signer")
	assert.Equal(t, "/usr/lib/naru/extensions/naru-orch/bin/naru-signer", signer.Binary)
	assert.Equal(t, &config.User{UID: 6001, GID: 6001}, signer.User)
	assert.Equal(t, []config.Dir{
		{Path: "/var/lib/naru/signer", Mode: "0700", UID: 6001, GID: 6001},
		{Path: "/run/naru-signer", Mode: "0750", UID: 6001, GID: 6001},
	}, signer.Dirs, "the directory of the keys is 0700 and the signer's")
	assert.Equal(t, []string{
		"serve", "--dir=/var/lib/naru/signer", "--socket=/run/naru-signer/signer.sock", "--socket-mode=0660",
		"--peer-listen=192.168.0.40:7600", "--peer-name=192.168.0.40", "--metrics-addr=192.168.0.40:9103",
	}, signer.Args)
	require.Len(t, signer.Setup, 1)
	assert.Equal(t, []string{"/var/lib/naru/signer/recovery-public.json"}, signer.Setup[0].Unless)
	assert.Equal(t, "init", signer.Setup[0].Args[0])

	orch := service(t, cfg, "naru-orchd")
	assert.Equal(t, "/usr/lib/naru/extensions/naru-orch/bin/naru-orchd", orch.Binary, "orch runs from the extension")
	assert.Equal(t, &config.User{UID: 6002, GID: 6002, Groups: []uint32{6001}}, orch.User, "orch's group is its own; the signer's group is the only one it joins")
	assert.NotContains(t, orch.User.Groups, uint32(0))
	assert.Equal(t, []config.Dir{{Path: "/var/lib/naru/orch", Mode: "0700", UID: 6002, GID: 6002}}, orch.Dirs)
	for _, want := range []string{
		"--name=host-1", "--initial-cluster=host-1=https://192.168.0.40:2380,host-2=https://192.168.0.41:2380",
		"--initial-cluster-state=new", "--listen=0.0.0.0:7500", "--san=192.168.0.40,127.0.0.1",
		"--key=/run/naru-orchd/orch.key", "--signer-socket=/run/naru-signer/signer.sock",
		"--recovery-public-key=/run/naru-orchd/recovery.pub", "--metrics-addr=192.168.0.40:9102",
		"--host-client-cert=/run/naru-orchd/orch.crt", "--host-client-key=/run/naru-orchd/orch.key", "--host-ca=/run/naru-orchd/host-ca.crt",
	} {
		assert.Contains(t, orch.Args, want)
	}
	assert.Equal(t, []string{"naru-orchd"}, file(t, cfg, config.OrchHostCAFile).Restart, "a changed host authority restarts orch")
	assert.Equal(t, []string{"/run/naru-orchd/orch.key"}, orch.RestartOnChange)

	assert.Equal(t, []string{"naru-hostd", "naru-signer", "naru-orchd"}, []string{cfg.Spec.Services[0].Name, cfg.Spec.Services[1].Name, cfg.Spec.Services[2].Name})
	assert.Equal(t, "192.168.0.40:9100", cfg.Spec.MetricsAddr())
	assert.Equal(t, &config.Owner{UID: 6002, GID: 6002}, file(t, cfg, config.OrchKeyFile).Owner)
	assert.Equal(t, []string{"naru-orchd"}, file(t, cfg, "/run/naru-orchd/orch.crt").Restart)
	file(t, cfg, "/run/naru-orchd/ca.crt")
	file(t, cfg, "/run/naru-orchd/revocation.pub")
	file(t, cfg, "/run/naru-orchd/recovery.pub")
	file(t, cfg, "/run/naru-signer/recovery.pub")
	assert.Contains(t, file(t, cfg, "/run/naru-signer/root.csr").Content, "CERTIFICATE REQUEST")
}

func TestParse_DerivedConfigurationParsesToItself(t *testing.T) {
	// A stored configuration is the dump of a parse, and comes back through Parse.
	first, err := config.Parse([]byte(daemonsDoc(t)))
	require.NoError(t, err)
	dumped, err := yaml.Dump(first)
	require.NoError(t, err)

	second, err := config.Parse(dumped)
	require.NoError(t, err)

	again, err := yaml.Dump(second)
	require.NoError(t, err)
	assert.Equal(t, string(dumped), string(again), "the flags and files are not added twice")
}

func TestParse_DerivedPartsFollowTheirSections(t *testing.T) {
	first, err := config.Parse([]byte(daemonsDoc(t)))
	require.NoError(t, err)
	dumped, err := yaml.Dump(first)
	require.NoError(t, err)

	// A patch that removes the sections is applied to the stored document, which still holds what they derived.
	var doc map[string]any
	require.NoError(t, yaml.Load(dumped, &doc))
	spec := doc["spec"].(map[string]any)
	delete(spec, "orch")
	delete(spec, "signer")
	delete(spec, "metrics")
	delete(spec["host"].(map[string]any), "network")
	delete(spec["host"].(map[string]any), "orch")
	rest, err := yaml.Dump(doc)
	require.NoError(t, err)

	cfg, err := config.Parse(rest)
	require.NoError(t, err)

	assert.Equal(t, []string{"naru-hostd"}, []string{cfg.Spec.Services[0].Name})
	assert.Len(t, cfg.Spec.Services, 1)
	assert.Equal(t, []string{"--listen=0.0.0.0:7443"}, cfg.Spec.Services[0].Args, "the flags init wrote go with their sections")
	for _, f := range cfg.Spec.Files {
		assert.NotContains(t, f.Path, "/run/naru-hostd/enroll.key")
		assert.NotContains(t, f.Path, "/run/naru-signer/")
		assert.NotContains(t, f.Path, "/run/naru-orchd/orch.crt")
		assert.NotContains(t, f.Path, config.OrchHostCAFile)
	}
}

func TestParse_WithoutSectionsAddsNothing(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))
	require.NoError(t, err)
	assert.Len(t, cfg.Spec.Services, 2)
	assert.Len(t, cfg.Spec.Files, 2)
	assert.Empty(t, cfg.Spec.MetricsAddr())
}

func TestParse_OrchWithoutBootstrapOrOptionalFields(t *testing.T) {
	doc := daemonsDoc(t)
	doc = strings.Replace(doc, "    bootstrap:\n", "    nobootstrap:\n", 1)
	_, err := config.Parse([]byte(doc))
	require.Error(t, err, "the control: a misspelt field is refused")

	cfg, err := config.Parse([]byte(removeBlock(t, daemonsDoc(t), "    bootstrap:\n", "  orch:\n    peerURL")))
	require.NoError(t, err)
	assert.Empty(t, service(t, cfg, "naru-signer").Setup, "a signer with no bootstrap has no setup")
	assert.NotContains(t, filePaths(cfg), "/run/naru-signer/root.csr")
}

func TestParse_OrchWithoutAHostAuthorityPresentsItsOwnPairAndTrustsItsAuthority(t *testing.T) {
	doc := daemonsDoc(t)
	i := strings.Index(doc, "    hostCA: |\n")
	j := strings.Index(doc, "    revocationPublicKey:")
	require.True(t, i >= 0 && j > i)
	cfg, err := config.Parse([]byte(doc[:i] + doc[j:]))
	require.NoError(t, err)
	args := service(t, cfg, "naru-orchd").Args
	assert.Contains(t, args, "--host-client-cert=/run/naru-orchd/orch.crt")
	assert.Contains(t, args, "--host-client-key=/run/naru-orchd/orch.key")
	for _, a := range args {
		assert.NotContains(t, a, "--host-ca", "orchd's --host-ca defaults to --ca")
	}
	assert.NotContains(t, filePaths(cfg), config.OrchHostCAFile)
}

func filePaths(cfg config.NodeConfig) []string {
	var out []string
	for _, f := range cfg.Spec.Files {
		out = append(out, f.Path)
	}
	return out
}

// removeBlock returns doc without the lines from the one that starts with from up to the one that starts with to.
func removeBlock(t *testing.T, doc, from, to string) string {
	t.Helper()
	i := strings.Index(doc, from)
	j := strings.Index(doc, to)
	require.True(t, i >= 0 && j > i)
	return doc[:i] + doc[j:]
}

func TestParse_DaemonSectionsRefuse(t *testing.T) {
	tests := []struct{ name, from, to, field string }{
		{"management prefix not a prefix", "managementPrefix: 192.168.0.0/24", "managementPrefix: nope", "spec.host.network.managementPrefix"},
		{"management prefix with host bits", "managementPrefix: 192.168.0.0/24", "managementPrefix: 192.168.0.1/24", "spec.host.network.managementPrefix"},
		{"egress interface too long", "egressInterface: eth0", "egressInterface: abcdefghijklmnopq", "spec.host.network.egressInterface"},
		{"tunnel port out of range", "tunnelPort: 51821", "tunnelPort: 70000", "spec.host.network.tunnelPort"},
		{"unknown network field", "egressInterface: eth0", "egressInterface: eth0\n      mtu: 1400", "spec.host.network.mtu"},
		{"host addresses are no longer a field", "egressInterface: eth0", "egressInterface: eth0\n      hostAddrs: [192.168.0.40]", "spec.host.network.hostAddrs"},
		{"peer names without peer listener", "    peerListen: 192.168.0.40:7600\n", "", "spec.signer.peerNames"},
		{"peer listener without names", "peerNames: [192.168.0.40]", "peerNames: []", "spec.signer.peerNames"},
		{"peer listener not an address", "peerListen: 192.168.0.40:7600", "peerListen: nope", "spec.signer.peerListen"},
		{"recovery key not JSON", `recoveryPublicKey: '{"x25519":"AA","ed25519":"AA"}'` + "\n      rootCSR", "recoveryPublicKey: nope\n      rootCSR", "spec.signer.bootstrap.recoveryPublicKey"},
		{"root request of another type", "-----BEGIN CERTIFICATE REQUEST-----", "-----BEGIN CERTIFICATE-----", "spec.signer.bootstrap.rootCSR"},
		{"orch peer URL not https", "peerURL: https://192.168.0.40:2380", "peerURL: http://192.168.0.40:2380", "spec.orch.peerURL"},
		{"orch cluster lacks this member", "      host-1: https://192.168.0.40:2380\n", "", "spec.orch.initialCluster"},
		{"orch cluster state", "sans:", "initialClusterState: old\n    sans:", "spec.orch.initialClusterState"},
		{"orch listen", "sans:", "listen: nope\n    sans:", "spec.orch.listen"},
		{"orch certificate not PEM", "    certificate: |\n        -----BEGIN CERTIFICATE-----", "    certificate: |\n        -----BEGIN NOTHING-----", "spec.orch.certificate"},
		{"orch host authority not PEM", "    hostCA: |\n        -----BEGIN CERTIFICATE-----", "    hostCA: |\n        -----BEGIN NOTHING-----", "spec.orch.hostCA"},
		{"orch name", "sans:", "name: Bad_Name\n    sans:", "spec.orch.name"},
		{"metrics listen not an address", "listen: 192.168.0.40\n  files", "listen: example.com\n  files", "spec.metrics.listen"},
		{"orch key file owned by root", "owner: {uid: 6002, gid: 6002}", "owner: {uid: 0, gid: 0}", "spec.files[0].owner"},
		{"orch key file readable by others", `mode: "0600"`, `mode: "0644"`, "spec.files[0].mode"},
		{"orch key file missing", "    - path: /run/naru-orchd/orch.key", "    - path: /run/naru-orchd/other.key", "spec.orch"},
		{"hostd flag given bare", `args: ["--listen=0.0.0.0:7443"]`, `args: ["--listen=0.0.0.0:7443", "--management-prefix"]`, "spec.services[0].args"},
		{"service user is root", "binary: /bin/naru-hostd", "binary: /bin/naru-hostd\n      user: {uid: 0, gid: 0}", "spec.services[0].user"},
		{"service dir relative", "binary: /bin/naru-hostd", "binary: /bin/naru-hostd\n      dirs: [{path: rel}]", "spec.services[0].dirs[0].path"},
		{"service dir mode", "binary: /bin/naru-hostd", "binary: /bin/naru-hostd\n      dirs: [{path: /var/lib/x, mode: '0999'}]", "spec.services[0].dirs[0].mode"},
		{"service setup without args", "binary: /bin/naru-hostd", "binary: /bin/naru-hostd\n      setup: [{unless: [/run/x]}]", "spec.services[0].setup[0].args"},
		{"service setup unless relative", "binary: /bin/naru-hostd", "binary: /bin/naru-hostd\n      setup: [{args: [x], unless: [rel]}]", "spec.services[0].setup[0].unless[0]"},
		{"hostd orch url not https", "url: https://192.168.0.40:7500", "url: http://192.168.0.40:7500", "spec.host.orch.url"},
		{"hostd orch digest short", "caSHA256: abababababababababababababababababababababababababababababababab", "caSHA256: abcd", "spec.host.orch.caSHA256"},
		{"hostd orch digest not hex", "caSHA256: abababababababababababababababababababababababababababababababab", "caSHA256: zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "spec.host.orch.caSHA256"},
		{"hostd orch key empty", "enrollKey: enroll-secret", "enrollKey: ''", "spec.host.orch.enrollKey"},
		{"hostd orch flag given bare", `args: ["--listen=0.0.0.0:7443"]`, `args: ["--listen=0.0.0.0:7443", "--orch-url"]`, "spec.services[0].args"},
		{"signer replica beside bootstrap", "    bootstrap:\n", "    replica: {}\n    bootstrap:\n", "spec.signer.replica"},
	}
	doc := daemonsDoc(t)
	_, err := config.Parse([]byte(doc))
	require.NoError(t, err, "the control: the document the cases change is accepted")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, doc, tt.from)
			_, err := config.Parse([]byte(strings.Replace(doc, tt.from, tt.to, 1)))
			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
}

func TestParse_OrchNeedsTheSignerAndHostNetworkNeedsHostd(t *testing.T) {
	doc := daemonsDoc(t)
	_, err := config.Parse([]byte(removeBlock(t, doc, "  signer:\n", "  orch:\n    peerURL")))
	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.orch")

	noHostd := strings.Replace(doc, "    - name: naru-hostd\n      binary: /bin/naru-hostd\n      args: [\"--listen=0.0.0.0:7443\"]\n", "    - name: other\n      binary: /bin/other\n", 1)
	_, err = config.Parse([]byte(noHostd))
	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.host.network")
}

func TestService_EqualSeesUserDirsAndSetup(t *testing.T) {
	base := config.Service{Name: "a", Binary: "/bin/a", User: &config.User{UID: 1, GID: 1}, Dirs: []config.Dir{{Path: "/x", Mode: "0700"}}, Setup: []config.Setup{{Args: []string{"init"}, Unless: []string{"/y"}}}}
	same := config.Service{Name: "a", Binary: "/bin/a", User: &config.User{UID: 1, GID: 1}, Dirs: []config.Dir{{Path: "/x", Mode: "0700"}}, Setup: []config.Setup{{Args: []string{"init"}, Unless: []string{"/y"}}}}
	assert.True(t, base.Equal(same))

	for name, change := range map[string]func(*config.Service){
		"user":        func(s *config.Service) { s.User = &config.User{UID: 2, GID: 1} },
		"no user":     func(s *config.Service) { s.User = nil },
		"groups":      func(s *config.Service) { s.User = &config.User{UID: 1, GID: 1, Groups: []uint32{5}} },
		"dir mode":    func(s *config.Service) { s.Dirs = []config.Dir{{Path: "/x", Mode: "0755"}} },
		"dir owner":   func(s *config.Service) { s.Dirs = []config.Dir{{Path: "/x", Mode: "0700", UID: 3}} },
		"setup args":  func(s *config.Service) { s.Setup = []config.Setup{{Args: []string{"other"}, Unless: []string{"/y"}}} },
		"setup unles": func(s *config.Service) { s.Setup = []config.Setup{{Args: []string{"init"}, Unless: []string{"/z"}}} },
		"setup step":  func(s *config.Service) { s.Setup = append(s.Setup, config.Setup{Args: []string{"join"}}) },
		"no setup":    func(s *config.Service) { s.Setup = nil },
	} {
		t.Run(name, func(t *testing.T) {
			other := same
			change(&other)
			assert.False(t, base.Equal(other), "a service that differs in %s is not equal, so an apply restarts it", name)
		})
	}
}

// replicaDoc is daemonsDoc with a signer that is a replica, which holds no bootstrap.
func replicaDoc(t *testing.T, join bool) string {
	cert, _, _ := throwaway(t)
	doc := removeBlock(t, daemonsDoc(t), "    bootstrap:\n", "  orch:\n    peerURL")
	doc = strings.Replace(doc, "    peerNames: [192.168.0.40]\n", "    peerNames: [192.168.0.40]\n    replica:\n", 1)
	if !join {
		return strings.Replace(doc, "    replica:\n", "    replica: {}\n", 1)
	}
	return strings.Replace(doc, "    replica:\n", `    replica:
      join:
        peer: 192.168.0.41:7600
        authorisation: '{"signed":"request"}'
        peerCA: |
`+strings.ReplaceAll(indent(cert), "        ", "          "), 1)
}

func TestParse_ASignerReplicaMakesItsJoinKeyAtFirstStartAndJoinsOnlyOnceAuthorised(t *testing.T) {
	const authorities = "/var/lib/naru/signer/recovery-public.json"

	first, err := config.Parse([]byte(replicaDoc(t, false)))
	require.NoError(t, err)
	svc := service(t, first, "naru-signer")
	require.Len(t, svc.Setup, 1, "before root authorises the key there is nothing to join with")
	assert.Equal(t, []string{"join-key", "--dir=/var/lib/naru/signer", "--out=/run/naru-signer/join.pub"}, svc.Setup[0].Args)
	assert.Equal(t, []string{"/var/lib/naru/signer/join.key", authorities}, svc.Setup[0].Unless, "neither again nor once the directory holds authorities")
	assert.NotContains(t, filePaths(first), "/run/naru-signer/replica.authorisation")

	second, err := config.Parse([]byte(replicaDoc(t, true)))
	require.NoError(t, err)
	svc = service(t, second, "naru-signer")
	require.Len(t, svc.Setup, 2)
	assert.Equal(t, "join-key", svc.Setup[0].Args[0], "the key is made before the join reads it")
	assert.Equal(t, []string{
		"join", "--dir=/var/lib/naru/signer", "--host-id=host-1", "--peer=https://192.168.0.41:7600",
		"--peer-ca=/run/naru-signer/peer-ca.crt", "--authorisation=/run/naru-signer/replica.authorisation",
	}, svc.Setup[1].Args, "the host ID is the Node's name")
	assert.Equal(t, []string{authorities}, svc.Setup[1].Unless, "join does not run again once the directory holds authorities")
	assert.JSONEq(t, `{"signed":"request"}`, file(t, second, "/run/naru-signer/replica.authorisation").Content)
	assert.Contains(t, file(t, second, "/run/naru-signer/peer-ca.crt").Content, "BEGIN CERTIFICATE")
	assert.NotContains(t, filePaths(second), "/run/naru-signer/root.csr")
}

func TestParse_ASignerReplicaJoinRefuses(t *testing.T) {
	doc := replicaDoc(t, true)
	_, err := config.Parse([]byte(doc))
	require.NoError(t, err, "the control: the document the cases change is accepted")
	for name, c := range map[string]struct{ from, to, field string }{
		"peer without a port":  {"peer: 192.168.0.41:7600", "peer: 192.168.0.41", "spec.signer.replica.join.peer"},
		"peer with a bad port": {"peer: 192.168.0.41:7600", "peer: 192.168.0.41:99999", "spec.signer.replica.join.peer"},
		"no authorisation":     {`authorisation: '{"signed":"request"}'`, "authorisation: ''", "spec.signer.replica.join.authorisation"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(strings.Replace(doc, c.from, c.to, 1)))
			require.Error(t, err)
			assert.Contains(t, fields(t, err), c.field)
		})
	}
}

func TestParse_ADerivedReplicaConfigurationParsesToItself(t *testing.T) {
	first, err := config.Parse([]byte(replicaDoc(t, true)))
	require.NoError(t, err)
	dumped, err := yaml.Dump(first)
	require.NoError(t, err)

	second, err := config.Parse(dumped)
	require.NoError(t, err)

	again, err := yaml.Dump(second)
	require.NoError(t, err)
	assert.Equal(t, string(dumped), string(again))
}

func TestParse_OrchRestoreGivesOrchdItsRestoreFlagAndNothingElseDoes(t *testing.T) {
	plain, err := config.Parse([]byte(daemonsDoc(t)))
	require.NoError(t, err)
	assert.NotContains(t, service(t, plain, "naru-orchd").Args, "--restore", "restore is off unless asked for")

	doc := strings.Replace(daemonsDoc(t), "  orch:\n    peerURL", "  orch:\n    restore: true\n    peerURL", 1)
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	assert.Contains(t, service(t, cfg, "naru-orchd").Args, "--restore")
	assert.NotContains(t, service(t, cfg, "naru-signer").Args, "--restore")

	_, err = config.Parse([]byte(strings.Replace(doc, "restore: true", "restore: maybe", 1)))
	require.Error(t, err, "restore is a boolean")
}

// joining is the daemons document with orch joining a running member instead of starting a cluster.
func joining(t *testing.T, extra string) string {
	t.Helper()
	doc := daemonsDoc(t)
	cluster := "    initialCluster:\n      host-1: https://192.168.0.40:2380\n      host-2: https://192.168.0.41:2380\n"
	require.Contains(t, doc, cluster)
	return strings.Replace(doc, cluster, "    join: https://192.168.0.41:2379\n"+extra, 1)
}

func TestParse_OrchJoinGivesOrchdItsJoinFlagAndTheExistingState(t *testing.T) {
	cfg, err := config.Parse([]byte(joining(t, "")))
	require.NoError(t, err)

	args := service(t, cfg, "naru-orchd").Args
	assert.Contains(t, args, "--join=https://192.168.0.41:2379")
	assert.Contains(t, args, "--initial-cluster-state=existing", "a member that joins is an existing member's, whatever the state says")
	for _, a := range args {
		assert.NotContains(t, a, "--initial-cluster=", "orchd takes the initial cluster from the cluster it joins")
	}

	// Control: a member that starts a cluster takes no --join.
	plain, err := config.Parse([]byte(daemonsDoc(t)))
	require.NoError(t, err)
	for _, a := range service(t, plain, "naru-orchd").Args {
		assert.NotContains(t, a, "--join")
	}

	_, err = config.Parse([]byte(joining(t, "    initialClusterState: existing\n")))
	require.NoError(t, err, "the state may be spelled out as existing")
}

func TestParse_OrchJoinIsRefusedBesideWhatItExcludesAndWhenItIsNotAnHTTPSURL(t *testing.T) {
	for name, tc := range map[string]struct{ doc, field string }{
		"restore":            {joining(t, "    restore: true\n"), "spec.orch.join"},
		"an initial cluster": {strings.Replace(joining(t, ""), "    join:", "    initialCluster:\n      host-1: https://192.168.0.40:2380\n    join:", 1), "spec.orch.initialCluster"},
		"the state new":      {joining(t, "    initialClusterState: new\n"), "spec.orch.initialClusterState"},
		"http":               {strings.Replace(joining(t, ""), "https://192.168.0.41:2379", "http://192.168.0.41:2379", 1), "spec.orch.join"},
		"no port":            {strings.Replace(joining(t, ""), "https://192.168.0.41:2379", "https://192.168.0.41", 1), "spec.orch.join"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
}
