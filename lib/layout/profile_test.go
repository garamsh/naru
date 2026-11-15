package layout_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func profile(t *testing.T) layout.HostProfile {
	t.Helper()
	f := layout.File{Path: "/x", SHA256: digest}
	return layout.HostProfile{
		Guest: layout.Manifest{
			Kernels: map[string]layout.File{"naru": f}, DefaultKernel: "naru", GuestInitrd: f, ConverterInitrd: f,
		},
		FirecrackerSHA256: digest,
		Orch:              layout.ProfileOrch{URL: "https://orch.example:8443", CASHA256: digest},
		MachinePool:       netip.MustParsePrefix("10.200.0.0/16"),
	}
}

func joined(t *testing.T) layout.JoinedHost {
	t.Helper()
	ca, err := pki.New("orch")
	require.NoError(t, err)
	host, err := pki.New("host")
	require.NoError(t, err)
	pair, err := host.Server("h-0123456789abcdef", net.ParseIP("192.168.0.20"))
	require.NoError(t, err)
	return layout.JoinedHost{
		ID: "h-0123456789abcdef", Address: netip.MustParseAddr("192.168.0.20"), Region: "eu-1",
		Labels:        map[string]string{"disk": "ssd"},
		MachinePrefix: netip.MustParsePrefix("10.200.1.0/24"),
		Certificate:   string(pair.CertPEM), OrchCA: string(ca.CertPEM),
	}
}

func TestHostProfileNode_BuildsAHostConfigWithNoGaonAndNoClusterSections(t *testing.T) {
	p := profile(t)
	h := joined(t)
	n, err := p.Node(h)
	require.NoError(t, err)
	data, err := n.UserData()
	require.NoError(t, err)

	var raw struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec map[string]any `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &raw))
	// init's join form named no host and chose the data disk; the configuration that replaces it
	// names the host by its ID and the disk by the label the layout formats (ADR 0058).
	assert.Equal(t, h.ID, raw.Metadata.Name)
	assert.Equal(t, map[string]any{"label": wire.DataLabel, "mountPoint": wire.DataMount}, raw.Spec["dataDisk"])
	for _, section := range []string{"signer", "orch", "join", "pki"} {
		assert.NotContains(t, raw.Spec, section)
	}
	host, ok := raw.Spec["host"].(map[string]any)
	require.True(t, ok)
	orch, ok := host["orch"].(map[string]any)
	require.True(t, ok, "the host keeps reporting to orch")
	assert.Equal(t, p.Orch.URL, orch["url"])

	parsed := parse(t, data)
	var names []string
	for _, s := range parsed.Spec.Services {
		names = append(names, s.Name)
		if s.Name == "naru-hostd" {
			i := slices.Index(s.Args, "--key")
			require.GreaterOrEqual(t, i, 0)
			assert.Equal(t, layout.HostKeyFile, s.Args[i+1], "the key stays where the host made it")
		}
	}
	slices.Sort(names)
	// A joined host has no inputs tree and so no naru-registry binary to run: a service for it would
	// fail to start and be restarted by init for ever.
	assert.Equal(t, []string{"mvmd", "naru-hostd"}, names)
	files := map[string]string{}
	for _, f := range parsed.Spec.Files {
		files[f.Path] = f.Content
	}
	assert.Equal(t, h.Certificate, files["/run/naru-hostd/tls.crt"])
	assert.Equal(t, h.OrchCA, files["/run/naru-hostd/client-ca.crt"])
	assert.NotContains(t, files, "/run/naru-hostd/tls.key", "a private key is never carried")
	assert.NotContains(t, string(data), "PRIVATE KEY")
}

func TestHostProfileNode_ServesTheNodeAPIFromTheHostCertificateAndItsKeyFile(t *testing.T) {
	p := profile(t)
	ca, err := pki.New("clients")
	require.NoError(t, err)
	p.APIClientCA = string(ca.CertPEM)
	h := joined(t)
	n, err := p.Node(h)
	require.NoError(t, err)
	data, err := n.UserData()
	require.NoError(t, err)

	var raw struct {
		Spec struct {
			API map[string]string `yaml:"api"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &raw))
	assert.Equal(t, p.APIClientCA, raw.Spec.API["clientCA"])
	assert.Equal(t, h.Certificate, raw.Spec.API["certificate"])
	assert.Equal(t, layout.HostKeyFile, raw.Spec.API["keyFile"])
	assert.NotContains(t, raw.Spec.API, "key")
}

func TestHostProfileNode_CarriesTheRevocationKeyAndListToHostdAndTheNodeAPI(t *testing.T) {
	p := profile(t)
	ca, err := pki.New("clients")
	require.NoError(t, err)
	p.APIClientCA = string(ca.CertPEM)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubPEM, err := identity.EncodeRevocationPublic(pub)
	require.NoError(t, err)
	list, err := identity.SignCRL(priv, identity.CRL{Seq: 4})
	require.NoError(t, err)
	h := joined(t)
	h.RevocationPublicKey, h.CRL = pubPEM, string(list)
	n, err := p.Node(h)
	require.NoError(t, err)
	data, err := n.UserData()
	require.NoError(t, err)

	var raw struct {
		Spec struct {
			Host map[string]any    `yaml:"host"`
			API  map[string]string `yaml:"api"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &raw))
	assert.Equal(t, pubPEM, raw.Spec.Host["revocationPublicKey"])
	assert.Equal(t, string(list), raw.Spec.Host["revocationList"])
	assert.Equal(t, pubPEM, raw.Spec.API["revocationPublicKey"])
}

func TestHostProfileNode_WithNoRevocationKeyCarriesNoneAndRefusesAListWithoutOne(t *testing.T) {
	p := profile(t)
	h := joined(t)
	n, err := p.Node(h)
	require.NoError(t, err)
	data, err := n.UserData()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "revocation")

	h.CRL = "{}"
	n, err = p.Node(h)
	require.NoError(t, err)
	_, err = n.UserData()
	assert.ErrorContains(t, err, "needs the key")
}

func TestParseHostProfile_RefusesWhatIsOneHostsOwn(t *testing.T) {
	good, err := profile(t).Marshal()
	require.NoError(t, err)
	_, err = layout.ParseHostProfile(good)
	require.NoError(t, err)

	for name, c := range map[string]struct {
		doc  string
		want string
	}{
		"a bootstrap token": {"token: abcdef.0123456789abcdef\n", "token"},
		"a CA key":          {"caKey: x\n", "caKey"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := layout.ParseHostProfile(append(slices.Clone(good), c.doc...))
			assert.ErrorContains(t, err, c.want)
		})
	}
	p := profile(t)
	p.APIClientCA = "-----BEGIN EC PRIVATE KEY-----\n"
	assert.ErrorContains(t, p.Check(), "private key")
}

func TestHostProfileCheck_RefusesAPoolThatCannotGiveEachHostAPrefix(t *testing.T) {
	for name, pool := range map[string]string{
		"smaller than a /24": "10.200.1.0/25",
		"host bits set":      "10.200.1.5/16",
		"the default route":  "0.0.0.0/0",
	} {
		t.Run(name, func(t *testing.T) {
			p := profile(t)
			p.MachinePool = netip.MustParsePrefix(pool)
			assert.Error(t, p.Check())
		})
	}
	p := profile(t)
	p.MachinePool = netip.Prefix{}
	assert.Error(t, p.Check())
}

func TestHostProfileNode_RefusesAPrefixOutsideThePool(t *testing.T) {
	h := joined(t)
	h.MachinePrefix = netip.MustParsePrefix("10.201.1.0/24")
	_, err := profile(t).Node(h)
	assert.ErrorContains(t, err, "not a /24 of the pool")
}

func TestHostProfileRedacted_ReplacesEverySecretAndKeepsTheShape(t *testing.T) {
	p := profile(t)
	p.Registries = []layout.Registry{
		{Host: "a.example", Username: "u", Password: "s3cret"},
		{Host: "b.example", Helper: "ecr-login"},
	}
	shown := p.Redacted()
	assert.Equal(t, []layout.Registry{
		{Host: "a.example", Username: "u", Password: layout.Redacted},
		{Host: "b.example", Helper: "ecr-login"},
	}, shown.Registries)
	assert.Equal(t, "s3cret", p.Registries[0].Password, "the profile itself is not changed")
	b, err := shown.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(b), "s3cret")
	_, err = layout.ParseHostProfile(b)
	assert.ErrorIs(t, err, layout.ErrRedacted, "what is shown cannot be set back")
}
