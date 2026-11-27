package machineapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

func TestPhase_AnUnknownPhaseReadsAsUnknown(t *testing.T) {
	var s machineapi.Status
	require.NoError(t, json.Unmarshal([]byte(`{"phase":"hibernating"}`), &s))
	assert.Equal(t, machineapi.PhaseUnknown, s.Phase)
	require.NoError(t, json.Unmarshal([]byte(`{"phase":"moving"}`), &s))
	assert.Equal(t, machineapi.PhaseMoving, s.Phase, "a phase the version names is read as it is")
	require.NoError(t, json.Unmarshal([]byte(`{"phase":"lost"}`), &s))
	assert.Equal(t, machineapi.PhaseLost, s.Phase)

	var snap machineapi.Snapshot
	require.NoError(t, json.Unmarshal([]byte(`{"phase":"archived"}`), &snap))
	assert.Equal(t, machineapi.SnapshotUnknown, snap.Phase)
	require.NoError(t, json.Unmarshal([]byte(`{"phase":"ready"}`), &snap))
	assert.Equal(t, machineapi.SnapshotReady, snap.Phase)
}

func TestCapabilities_AnUnknownCapabilityReadsAsAbsent(t *testing.T) {
	var c machineapi.Capabilities
	require.NoError(t, json.Unmarshal([]byte(`{"snapshot":["volumes","disk-delta"],"move":["teleport","cold"],"resize":["cpu","gpus"],"gpu":{"model":"L4","count":1}}`), &c))
	assert.Equal(t, machineapi.Capabilities{
		Snapshot: []machineapi.SnapshotKind{machineapi.SnapshotVolumes},
		Move:     []machineapi.MoveKind{machineapi.MoveCold},
		Resize:   []machineapi.ResizeKind{machineapi.ResizeCPU},
		GPU:      &machineapi.GPU{Model: "L4", Count: 1},
	}, c)
}

func validSpec() machineapi.Spec {
	return machineapi.Spec{
		Image:         "registry.example.test/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CPU:           2,
		MemoryBytes:   1 << 30,
		RootDiskBytes: 1 << 30,
		Volumes:       []machineapi.Volume{{Name: "data", SizeBytes: 1 << 30, MountPath: "/data"}},
		Env:           map[string]string{"A": "1"},
		Files:         []machineapi.File{{Path: "/etc/app/conf", Mode: 0o600, Content: []byte("x")}},
		Services:      []machineapi.Service{{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP}},
		NetworkPolicy: machineapi.NetworkPolicy{Egress: []machineapi.Rule{{Prefix: "0.0.0.0/0", Protocol: "tcp", Ports: []machineapi.PortRange{{First: 443, Last: 443}}}}},
	}
}

func TestSpecCheck_NamesTheFieldAtFault(t *testing.T) {
	require.NoError(t, validSpec().Check(), "the valid spec every case changes one field of")
	cases := []struct {
		field  string
		change func(s *machineapi.Spec)
	}{
		{"spec.image", func(s *machineapi.Spec) { s.Image = "registry.example.test/app:latest" }},
		{"spec.cpu", func(s *machineapi.Spec) { s.CPU = 0 }},
		{"spec.memoryBytes", func(s *machineapi.Spec) { s.MemoryBytes = 0 }},
		{"spec.rootDiskBytes", func(s *machineapi.Spec) { s.RootDiskBytes = 0 }},
		{"spec.state", func(s *machineapi.Spec) { s.State = "paused" }},
		{"spec.volumes[1].name", func(s *machineapi.Spec) {
			s.Volumes = append(s.Volumes, machineapi.Volume{Name: "data", SizeBytes: 1, MountPath: "/other"})
		}},
		{"spec.volumes[0].mountPath", func(s *machineapi.Spec) { s.Volumes[0].MountPath = "/proc/data" }},
		{"spec.env", func(s *machineapi.Spec) { s.Env = map[string]string{"A=B": "1"} }},
		{"spec.workingDir", func(s *machineapi.Spec) { s.WorkingDir = "relative" }},
		{"spec.files[0].mode", func(s *machineapi.Spec) { s.Files[0].Mode = 0o17777 }},
		{"spec.files[0].sizeBytes", func(s *machineapi.Spec) { s.Files[0].SizeBytes = 1 }},
		{"spec.services[0].name", func(s *machineapi.Spec) { s.Services[0].Name = "web--1" }},
		{"spec.services[0].protocol", func(s *machineapi.Spec) { s.Services[0].Protocol = "udp" }},
		{"spec.services[0].port", func(s *machineapi.Spec) { s.Services[0].Port = 0 }},
		{"spec.networkPolicy.egress[0].prefix", func(s *machineapi.Spec) { s.NetworkPolicy.Egress[0].Prefix = "10.0.0.1/8" }},
		{"spec.networkPolicy.egress[0].ports", func(s *machineapi.Spec) { s.NetworkPolicy.Egress[0].Protocol = "icmp" }},
		{"spec.cpu", func(s *machineapi.Spec) { s.Shape.GPU = &machineapi.GPU{Model: "L4", Count: 1} }},
		{"spec.memoryBytes", func(s *machineapi.Spec) {
			s.CPU, s.MemoryBytes, s.Shape.GPU = 8, 1<<30+1, &machineapi.GPU{Model: "L4", Count: 1}
		}},
	}
	for _, tc := range cases {
		s := validSpec()
		tc.change(&s)
		err := s.Check()
		var fe *machineapi.FieldError
		require.True(t, errors.As(err, &fe), "%s: refused with a FieldError, got %v", tc.field, err)
		assert.Equal(t, tc.field, fe.Field)
		assert.ErrorIs(t, err, machineapi.ErrInvalid)
	}
}

func TestSameSpec_LeavesOutTheStateAndNothingElse(t *testing.T) {
	a, b := validSpec(), validSpec()
	b.State = machineapi.StateStopped
	assert.True(t, machineapi.SameSpec(a, b), "the state is left out")
	b.Env = map[string]string{"A": "2"}
	assert.False(t, machineapi.SameSpec(a, b), "an env value is compared")
	a.Env, b.Env = map[string]string{}, nil
	assert.True(t, machineapi.SameSpec(a, b), "an empty map is an absent one")
}

func TestRedact_AnswersNoEnvValueAndNoFileContent(t *testing.T) {
	s := validSpec()
	r := s.Redact()
	assert.Equal(t, map[string]string{"A": machineapi.Redacted}, r.Env)
	assert.Equal(t, []machineapi.File{{Path: "/etc/app/conf", Mode: 0o600, SizeBytes: 1}}, r.Files)
	assert.Equal(t, "1", s.Env["A"], "the spec redacted from is unchanged")
	assert.Equal(t, []byte("x"), s.Files[0].Content)
}

func TestAliasInZone_FollowsTheZonesRules(t *testing.T) {
	const zone = "m.example.test"
	for host, inZone := range map[string]bool{"app." + zone: true, "app.workspaces.example.test": false} {
		got, err := machineapi.AliasInZone(host, zone)
		require.NoError(t, err, host)
		assert.Equal(t, inZone, got, host)
	}
	for _, host := range []string{"0123456789abcdef." + zone, "web--app." + zone, "a.b." + zone, zone, "App." + zone, "a_b.example.test", ""} {
		_, err := machineapi.AliasInZone(host, zone)
		assert.ErrorIs(t, err, machineapi.ErrAliasForm, host)
	}
}

func TestCheckAliasCertificate_RefusesAPairThatDoesNotServeTheHostname(t *testing.T) {
	now := time.Now()
	cert, key := selfSigned(t, "app.example.test", now)
	got, err := machineapi.CheckAliasCertificate("app.example.test", cert, key, now)
	require.NoError(t, err, "the pair for the hostname")
	assert.Equal(t, []string{"app.example.test"}, got.Names)

	_, err = machineapi.CheckAliasCertificate("other.example.test", cert, key, now)
	assert.ErrorIs(t, err, machineapi.ErrAliasCertificate, "another hostname")
	_, err = machineapi.CheckAliasCertificate("app.example.test", cert, key, now.Add(48*time.Hour))
	assert.ErrorIs(t, err, machineapi.ErrAliasCertificate, "after it expires")
	_, otherKey := selfSigned(t, "app.example.test", now)
	_, err = machineapi.CheckAliasCertificate("app.example.test", cert, otherKey, now)
	assert.ErrorIs(t, err, machineapi.ErrAliasCertificate, "a key of another certificate")
}

func selfSigned(t *testing.T, host string, now time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}
