package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/config"
)

// proxyDoc is a host that runs hostd and naru-proxy.
func proxyDoc(t *testing.T) string {
	cert, _, _ := throwaway(t)
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
  proxy:
    listen: ["0.0.0.0:443", "192.168.0.40:8443"]
    routeMaxAge: 30m
    certificate: |
` + indent(cert) + `  files:
    - path: /run/naru-proxy/zone.key
      mode: "0600"
      content: not-a-real-key
      owner: {uid: 6003, gid: 6003}
`
}

func TestParse_ProxyRunsNaruProxyAsItsUserAbleToBindPort443(t *testing.T) {
	cfg, err := config.Parse([]byte(proxyDoc(t)))
	require.NoError(t, err)

	p := service(t, cfg, "naru-proxy")
	assert.Equal(t, "/bin/naru-proxy", p.Binary)
	assert.Equal(t, []string{
		"--listen=0.0.0.0:443,192.168.0.40:8443", "--cert=/run/naru-proxy/zone.crt", "--key=/run/naru-proxy/zone.key",
		"--routes-file=/var/lib/naru/proxy-routes/routes.json", "--revision-file=/run/naru-proxy/revision",
		"--health-addr=127.0.0.1:7480", "--route-max-age=30m",
	}, p.Args)
	assert.Equal(t, &config.User{UID: 6003, GID: 6003}, p.User)
	assert.True(t, p.BindLowPorts)
	assert.Equal(t, []config.Dir{
		{Path: "/run/naru-proxy", Mode: "0755", UID: 6003, GID: 6003},
		{Path: "/var/lib/naru/proxy-routes", Mode: "0750", UID: 0, GID: 6003},
	}, p.Dirs, "the proxy writes its revision in its own directory and reads the routes hostd writes in its group")
	assert.Equal(t, []string{config.ProxyKeyFile}, p.RestartOnChange)
	crt := file(t, cfg, config.ProxyCertFile)
	assert.Contains(t, crt.Content, "BEGIN CERTIFICATE")
	assert.Equal(t, []string{"naru-proxy"}, crt.Restart)

	dumped, err := yaml.Dump(cfg)
	require.NoError(t, err)
	again, err := config.Parse(dumped)
	require.NoError(t, err)
	redumped, err := yaml.Dump(again)
	require.NoError(t, err)
	assert.Equal(t, string(dumped), string(redumped), "a stored configuration parses to itself")
}

func TestParse_ProxyListensOn443WhenNoAddressIsGiven(t *testing.T) {
	doc := strings.Replace(proxyDoc(t), `    listen: ["0.0.0.0:443", "192.168.0.40:8443"]`+"\n", "", 1)
	doc = strings.Replace(doc, "    routeMaxAge: 30m\n", "", 1)
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	args := service(t, cfg, "naru-proxy").Args
	assert.Contains(t, args, "--listen=0.0.0.0:443")
	for _, a := range args {
		assert.NotContains(t, a, "--route-max-age", "the proxy's own default")
	}
}

func TestParse_ProxyRefuses(t *testing.T) {
	tests := []struct{ name, from, to, field string }{
		{"listen not an address", `"192.168.0.40:8443"`, `"proxy.example:8443"`, "spec.proxy.listen[1]"},
		{"certificate not PEM", "-----BEGIN CERTIFICATE-----", "-----BEGIN NOTHING-----", "spec.proxy.certificate"},
		{"route max age not a duration", "routeMaxAge: 30m", "routeMaxAge: soon", "spec.proxy.routeMaxAge"},
		{"route max age not positive", "routeMaxAge: 30m", "routeMaxAge: 0s", "spec.proxy.routeMaxAge"},
		{"no hostd", "    - name: naru-hostd\n      binary: /bin/naru-hostd", "    - name: other\n      binary: /bin/other", "spec.proxy"},
		{"key file missing", "- path: /run/naru-proxy/zone.key", "- path: /run/naru-proxy/other.key", "spec.proxy"},
		{"key file readable by others", `mode: "0600"`, `mode: "0644"`, "spec.files[0].mode"},
		{"key file not the proxy's", "owner: {uid: 6003, gid: 6003}", "owner: {uid: 6002, gid: 6002}", "spec.files[0].owner"},
	}
	doc := proxyDoc(t)
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

func TestParse_BindLowPortsNeedsAUser(t *testing.T) {
	doc := proxyDoc(t)
	withUser := strings.Replace(doc, "      args: [\"--listen=0.0.0.0:7443\"]", "      args: [\"--listen=0.0.0.0:7443\"]\n      user: {uid: 7000, gid: 7000}\n      bindLowPorts: true", 1)
	_, err := config.Parse([]byte(withUser))
	require.NoError(t, err, "control: a user's service may bind a low port")
	asRoot := strings.Replace(doc, "      args: [\"--listen=0.0.0.0:7443\"]", "      args: [\"--listen=0.0.0.0:7443\"]\n      bindLowPorts: true", 1)
	_, err = config.Parse([]byte(asRoot))
	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.services[0].bindLowPorts")
}

func TestService_EqualSeesBindLowPorts(t *testing.T) {
	a := config.Service{Name: "x", Binary: "/bin/x", User: &config.User{UID: 7000, GID: 7000}}
	b := a
	assert.True(t, a.Equal(b), "control")
	b.BindLowPorts = true
	assert.False(t, a.Equal(b))
}

func TestParse_TheProxyServiceAndCertificateGoWithTheSection(t *testing.T) {
	first, err := config.Parse([]byte(proxyDoc(t)))
	require.NoError(t, err)
	dumped, err := yaml.Dump(first)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Load(dumped, &doc))
	delete(doc["spec"].(map[string]any), "proxy")
	var files []any
	for _, f := range doc["spec"].(map[string]any)["files"].([]any) {
		if f.(map[string]any)["path"] != config.ProxyKeyFile {
			files = append(files, f)
		}
	}
	doc["spec"].(map[string]any)["files"] = files
	rest, err := yaml.Dump(doc)
	require.NoError(t, err)
	require.Contains(t, string(rest), "naru-proxy", "the stored document still holds what the section derived")

	cfg, err := config.Parse(rest)
	require.NoError(t, err)
	assert.Len(t, cfg.Spec.Services, 1)
	assert.Equal(t, "naru-hostd", cfg.Spec.Services[0].Name)
	assert.Empty(t, cfg.Spec.Files)
}
