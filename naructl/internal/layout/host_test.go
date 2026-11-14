package layout_test

import (
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

type parsed struct {
	Spec struct {
		Host     map[string]any `yaml:"host"`
		Services []struct {
			Name string            `yaml:"name"`
			Args []string          `yaml:"args"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"services"`
		Files []struct {
			Path    string `yaml:"path"`
			Mode    string `yaml:"mode"`
			Content string `yaml:"content"`
		} `yaml:"files"`
	} `yaml:"spec"`
}

func parse(t *testing.T, data []byte) parsed {
	t.Helper()
	var p parsed
	require.NoError(t, yaml.Unmarshal(data, &p))
	return p
}

func hostNode(t *testing.T) (layout.Node, *pki.CA) {
	t.Helper()
	ca, err := pki.New("host")
	require.NoError(t, err)
	return node(t, func(n *layout.Node) {
		n.Host = &layout.Host{CA: ca, Region: "eu-1", Labels: map[string]string{"disk": "ssd"}}
	}), ca
}

func TestUserData_HostRunsNaruHostdWithAPairFromTheHostAuthorityAndNeverItsKey(t *testing.T) {
	n, ca := hostNode(t)
	data, err := n.UserData()
	require.NoError(t, err)
	p := parse(t, data)

	assert.Equal(t, map[string]any{"region": "eu-1", "labels": map[string]any{"disk": "ssd"}}, p.Spec.Host)
	var names []string
	for _, s := range p.Spec.Services {
		names = append(names, s.Name)
		if s.Name == "naru-hostd" {
			assert.Contains(t, s.Args, "/run/naru-hostd/client-ca.crt")
			assert.Equal(t, map[string]string{"PATH": "/bin"}, s.Env, "init passes no PATH of its own, and hostd runs nft")
			j := slices.Index(s.Args, "--host-id")
			require.GreaterOrEqual(t, j, 0, "naru-hostd would enroll under the kernel's hostname, which is (none)")
			assert.Equal(t, n.Name, s.Args[j+1])
			k := slices.Index(s.Args, "--advertise")
			require.GreaterOrEqual(t, k, 0, "orch would call 0.0.0.0, which --listen names")
			assert.Equal(t, n.Address.Addr().String()+":"+layout.HostPort, s.Args[k+1])
			i := slices.Index(s.Args, "--machine-gateway")
			require.GreaterOrEqual(t, i, 0, "naru-hostd is not told the gateway of the Machines' network")
			assert.Equal(t, "10.200.1.1", s.Args[i+1], "the first address of the Machine prefix, which mvmd's own gateway is")
		}
	}
	require.Contains(t, names, "naru-hostd")
	assert.Equal(t, "mvmd", names[0])
	assert.Equal(t, "naru-hostd", names[1], "after mvmd, which it dials")

	files := map[string]string{}
	modes := map[string]string{}
	for _, f := range p.Spec.Files {
		files[f.Path], modes[f.Path] = f.Content, f.Mode
	}
	assert.Equal(t, "0600", modes["/run/naru-hostd/tls.key"])
	assert.Equal(t, string(ca.CertPEM), files["/run/naru-hostd/client-ca.crt"])
	block, _ := pem.Decode([]byte(files["/run/naru-hostd/tls.crt"]))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(parseCA(t, ca))
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.NoError(t, err, "the serving certificate is the host authority's")
	assert.Equal(t, "192.168.0.10", cert.IPAddresses[0].String())
	for path, content := range files {
		assert.NotContains(t, content, string(ca.KeyPEM), "the authority's key is not carried in %s", path)
	}
}

func parseCA(t *testing.T, ca *pki.CA) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(ca.CertPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c
}

func TestUserData_ANodeWithoutHostRunsNoNaruHostd(t *testing.T) {
	data, err := node(t, func(*layout.Node) {}).UserData()
	require.NoError(t, err)
	p := parse(t, data)

	assert.Nil(t, p.Spec.Host)
	for _, s := range p.Spec.Services {
		assert.NotEqual(t, "naru-hostd", s.Name)
	}
	for _, f := range p.Spec.Files {
		assert.NotContains(t, f.Path, "naru-hostd")
	}
	assert.NotContains(t, string(data), "host:")
}

func TestUserData_RefusesAHostInitWouldRefuse(t *testing.T) {
	for name, change := range map[string]func(*layout.Host){
		"a region that is no DNS label": func(h *layout.Host) { h.Region = "EU_1" },
		"no region":                     func(h *layout.Host) { h.Region = "" },
		"a label value with a space":    func(h *layout.Host) { h.Labels["disk"] = "a b" },
		"a label key with two slashes":  func(h *layout.Host) { h.Labels["a/b/c"] = "x" },
		"no authority":                  func(h *layout.Host) { h.CA = nil },
	} {
		t.Run(name, func(t *testing.T) {
			n, _ := hostNode(t)
			change(n.Host)
			_, err := n.UserData()
			assert.Error(t, err)
		})
	}
	n, _ := hostNode(t)
	n.Host.Labels["example.com/tier"] = "gold"
	_, err := n.UserData()
	assert.NoError(t, err, "the control: a prefixed key is a label key")
}
