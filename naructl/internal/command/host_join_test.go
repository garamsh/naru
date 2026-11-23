package command_test

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/orchapi"
)

const joinSecret = "naru-ek-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// writeKeyFile writes the enrollment key to a file, as host install-seed reads it.
func writeKeyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "enroll.key")
	require.NoError(t, os.WriteFile(p, []byte(joinSecret+"\n"), 0o600))
	return p
}

func TestHostInstallSeed_WritesTheSeedAt0600WithTheKeyInItAndNowhereElse(t *testing.T) {
	out := filepath.Join(t.TempDir(), "seed.iso")
	stdout, err := run(t, "host", "install-seed", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", d("a"),
		"--enroll-key", writeKeyFile(t), "--name", "rack-3", "--out", out)
	require.NoError(t, err, stdout)

	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the ISO carries the key")
	img, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(img), "enrollKey: "+joinSecret)
	assert.Contains(t, string(img), "caSHA256: "+d("a"))
	assert.NotContains(t, stdout, joinSecret, "the key is printed nowhere")
}

func TestHostInstallSeed_Refusals(t *testing.T) {
	good := func(extra ...string) []string {
		return append([]string{"host", "install-seed", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", d("a"),
			"--enroll-key", writeKeyFile(t), "--out", filepath.Join(t.TempDir(), "seed.iso")}, extra...)
	}
	_, err := run(t, good()...)
	require.NoError(t, err, "control")

	for name, args := range map[string][]string{
		"the key on the command line": {"host", "install-seed", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", d("a"), "--enroll-key", joinSecret, "--out", filepath.Join(t.TempDir(), "x.iso")},
		"http":                        {"host", "install-seed", "--orch-url", "http://orch.example:8443", "--orch-ca-sha256", d("a"), "--enroll-key", writeKeyFile(t), "--out", filepath.Join(t.TempDir(), "x.iso")},
		"a short digest":              {"host", "install-seed", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", "abcd", "--enroll-key", writeKeyFile(t), "--out", filepath.Join(t.TempDir(), "x.iso")},
		"no out":                      {"host", "install-seed", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", d("a"), "--enroll-key", writeKeyFile(t)},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := run(t, args...)
			require.Error(t, err)
			assert.NotContains(t, out, joinSecret)
		})
	}
}

// profileFlags are the flags of a minimal gen host-profile.
func profileFlags(t *testing.T, out string, extra ...string) []string {
	t.Helper()
	return append([]string{"gen", "host-profile", "--firecracker-sha256", d("7"),
		"--machine-pool", "10.200.0.0/16", "--orch-url", "https://orch.example:8443", "--orch-ca-sha256", d("b"), "--out", out}, extra...)
}

func TestGenHostProfile_WritesAProfileOrchCanBuildAHostFromThatInitAccepts(t *testing.T) {
	out := filepath.Join(t.TempDir(), "profile.yaml")
	stdout, err := run(t, profileFlags(t, out)...)
	require.NoError(t, err, stdout)
	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "no password in it")

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	p, err := layout.ParseHostProfile(b)
	require.NoError(t, err)
	assert.Equal(t, netip.MustParsePrefix("10.200.0.0/16"), p.MachinePool)

	// The configuration orch builds from it, for one host, is one naru-init validates: the same
	// check the host applies to it after the handoff.
	ca, err := pki.New("orch")
	require.NoError(t, err)
	host, err := pki.New("host")
	require.NoError(t, err)
	pair, err := host.Server("h-0123456789abcdef", net.ParseIP("192.168.0.20"))
	require.NoError(t, err)
	n, err := p.Node(layout.JoinedHost{
		ID: "h-0123456789abcdef", Address: netip.MustParseAddr("192.168.0.20"), Region: "eu-1",
		MachinePrefix: netip.MustParsePrefix("10.200.1.0/24"), Certificate: string(pair.CertPEM), OrchCA: string(ca.CertPEM),
	})
	require.NoError(t, err)
	data, err := n.UserData()
	require.NoError(t, err)
	cfg := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(cfg, data, 0o600))
	assert.Contains(t, validate(t, cfg), "valid; node=h-0123456789abcdef")
}

func TestGenHostProfile_AProfileWithARegistryPasswordIsKept0600(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	require.NoError(t, os.WriteFile(pw, []byte("hunter2\n"), 0o600))
	out := filepath.Join(t.TempDir(), "profile.yaml")
	stdout, err := run(t, profileFlags(t, out, "--registry", "host=registry.example:5000,username=ci,password-file="+pw)...)
	require.NoError(t, err, stdout)
	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NotContains(t, stdout, "hunter2")
}

func TestGenHostProfile_Refusals(t *testing.T) {
	for name, extra := range map[string][]string{
		"a pool of one host":  {"--machine-pool", "10.200.0.0/25"},
		"host bits in a pool": {"--machine-pool", "10.200.1.0/16"},
		"a lone aws flag":     {"--aws-region", "us-east-1"},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "profile.yaml")
			stdout, err := run(t, profileFlags(t, out, extra...)...)
			require.Error(t, err, stdout)
			_, statErr := os.Stat(out)
			assert.True(t, os.IsNotExist(statErr), "a refused profile writes no file")
		})
	}
}

func TestHostProfile_SetSendsTheCheckedFileAndGetReturnsIt(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	file := filepath.Join(t.TempDir(), "profile.yaml")
	mustRun(t, profileFlags(t, file)...)
	want, err := os.ReadFile(file)
	require.NoError(t, err)

	out, err := run(t, op.args(f, "host", "profile", "set", "-f", file)...)
	require.NoError(t, err, out)
	assert.Equal(t, want, f.profile)

	out, err = run(t, op.args(f, "host", "profile", "get")...)
	require.NoError(t, err, out)
	assert.Equal(t, string(want), out)

	// A profile naructl can see is wrong is not sent.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("machinePool: 10.0.0.0/8\nsurprise: 1\n"), 0o600))
	f.profile = nil
	_, err = run(t, op.args(f, "host", "profile", "set", "-f", bad)...)
	require.Error(t, err)
	assert.Nil(t, f.profile)
}

func TestHostRemove_SendsTheIDAndSaysTheCertificateIsStillValid(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	out, err := run(t, op.args(f, "host", "remove", "h-0123456789abcdef")...)
	require.NoError(t, err, out)
	assert.Equal(t, []string{"h-0123456789abcdef"}, f.removed)
	assert.Contains(t, out, "still valid")
	assert.Contains(t, out, "operator revoke")

	_, err = run(t, op.args(f, "host", "remove")...)
	require.Error(t, err, "no host ID")
}

func TestHostPending_ShowsWhatTheHostReportedOfItself(t *testing.T) {
	pub := hostKey(t)
	p := pendingHost("h-aaaa", pub)
	p.Hardware = &orchapi.Hardware{CPUs: 8, MemoryBytes: 16 << 30,
		Disks: []orchapi.Disk{{Name: "sda", Size: 1 << 40}, {Name: "sdb", Size: 1 << 40}},
		Links: []orchapi.Link{{Name: "eth0", MAC: "52:54:00:12:34:56", Carrier: true}}}
	f := newFakeOrch(t, p, pendingHost("h-bbbb", pub))
	op := newOperator(t, f, "operator")
	out, err := run(t, op.args(f, "host", "pending")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "8 CPU, 16384 MiB, 2 disk(s), 1 link(s)")
	assert.Equal(t, 1, strings.Count(out, "CPU,"), "a host that reported none shows a dash")
}

func TestHostProfile_GetPrintsWhatOrchReturnsAndSetRefusesTheRedactionItShows(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	file := filepath.Join(t.TempDir(), "profile.yaml")
	mustRun(t, profileFlags(t, file, "--registry", "host=registry.example:5000,username=robot,password-file="+writeSecret(t, "hunter2"))...)
	written, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Contains(t, string(written), "hunter2", "the file naructl writes holds the password; orch keeps it and shows it to no one")

	// What orch returns has the password redacted; get prints that and nothing more.
	p, err := layout.ParseHostProfile(written)
	require.NoError(t, err)
	shown, err := p.Redacted().Marshal()
	require.NoError(t, err)
	f.profile = shown
	out, err := run(t, op.args(f, "host", "profile", "get")...)
	require.NoError(t, err, out)
	assert.Equal(t, string(shown), out)
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, layout.Redacted)

	// A redacted profile written back is refused before it is sent.
	back := filepath.Join(t.TempDir(), "back.yaml")
	require.NoError(t, os.WriteFile(back, shown, 0o600))
	f.profile = nil
	_, err = run(t, op.args(f, "host", "profile", "set", "-f", back)...)
	require.ErrorIs(t, err, layout.ErrRedacted)
	assert.Nil(t, f.profile)
}

func writeSecret(t *testing.T, v string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte(v+"\n"), 0o600))
	return path
}
