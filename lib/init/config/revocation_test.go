package config_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/init/config"
)

// revocationKeys returns the PEM of a new revocation public key and a list its private half signed.
func revocationKeys(t *testing.T) (pub, list string) {
	t.Helper()
	k, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pub, err = identity.EncodeRevocationPublic(k)
	require.NoError(t, err)
	b, err := identity.SignCRL(priv, identity.CRL{Seq: 3, IssuedAt: time.Now(), Serials: []string{"1f"}})
	require.NoError(t, err)
	return pub, string(b)
}

// joinedHostDoc is a Node that enrolls with orch, without a signer, and checks revocation.
func joinedHostDoc(t *testing.T, extra string) string {
	pub, list := revocationKeys(t)
	return `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: h-0123456789abcdef
spec:
  services:
    - name: naru-hostd
      binary: /bin/naru-hostd
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  host:
    region: seoul
    orch:
      url: https://192.168.0.40:7500
      caSHA256: abababababababababababababababababababababababababababababababab
      enrollKey: enroll-secret
    revocationPublicKey: |
` + strings.ReplaceAll(indent(pub), "        ", "      ") + `    revocationList: '` + list + `'
` + extra
}

func TestParse_HostRevocationGivesHostdItsKeyListAndSource(t *testing.T) {
	cfg, err := config.Parse([]byte(joinedHostDoc(t, "    revocationMaxAge: 5m\n")))
	require.NoError(t, err)

	hostd := service(t, cfg, "naru-hostd")
	assert.Contains(t, hostd.Args, "--revocation-public-key=/run/naru-hostd/revocation.pub")
	assert.Contains(t, hostd.Args, "--revocation-max-age=5m0s")
	assert.Contains(t, hostd.Args, "--revocation-seed=/run/naru-hostd/crl-seed.json")
	assert.Contains(t, hostd.Args, "--orch-url=https://192.168.0.40:7500", "without a signer the list comes from orch")
	for _, a := range hostd.Args {
		assert.NotContains(t, a, "--revocation-signer-socket", "no signer on this Node")
	}
	assert.Contains(t, file(t, cfg, config.HostdRevocationKeyFile).Content, "PUBLIC KEY")
	assert.Equal(t, []string{"naru-hostd"}, file(t, cfg, config.HostdRevocationKeyFile).Restart)
	assert.Contains(t, file(t, cfg, config.HostdRevocationSeedFile).Content, "signature")
}

func TestParse_ADerivedRevocationConfigurationParsesToItself(t *testing.T) {
	cfg, err := config.Parse([]byte(joinedHostDoc(t, "")))
	require.NoError(t, err)
	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	again, err := config.Parse(out)
	require.NoError(t, err)
	assert.Equal(t, service(t, cfg, "naru-hostd").Args, service(t, again, "naru-hostd").Args, "the flags and files it derived are replaced, not added to")
	assert.Equal(t, cfg.Spec.Files, again.Spec.Files)
}

func TestParse_ASignerNodeReadsTheListFromItsSocket(t *testing.T) {
	doc := daemonsDoc(t)
	pub, _ := revocationKeys(t)
	doc = strings.Replace(doc, "      enrollKey: enroll-secret\n", "      enrollKey: enroll-secret\n    revocationPublicKey: |\n"+strings.ReplaceAll(indent(pub), "        ", "      "), 1)
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	assert.Contains(t, service(t, cfg, "naru-hostd").Args, "--revocation-signer-socket=/run/naru-signer/signer.sock")
}

func TestParse_RevocationRefuses(t *testing.T) {
	pub, list := revocationKeys(t)
	other, _ := revocationKeys(t)
	host := func(key, extra string) string {
		return `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: naru-hostd
      binary: /bin/naru-hostd
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  host:
    region: seoul
` + key + extra
	}
	keyOf := func(p string) string {
		return "    revocationPublicKey: |\n" + strings.ReplaceAll(indent(p), "        ", "      ")
	}
	orch := "    orch:\n      url: https://192.168.0.40:7500\n      caSHA256: abababababababababababababababababababababababababababababababab\n      enrollKey: k\n"
	tests := []struct{ name, doc, field string }{
		{"a key with no source", host(keyOf(pub), ""), "spec.host.revocationPublicKey"},
		{"a key that is not PEM", host("    revocationPublicKey: nope\n", orch), "spec.host.revocationPublicKey"},
		{"a max age below a minute", host(keyOf(pub), orch+"    revocationMaxAge: 10s\n"), "spec.host.revocationMaxAge"},
		{"a max age with no key", host("", orch+"    revocationMaxAge: 5m\n"), "spec.host.revocationMaxAge"},
		{"a list signed by another key", host(keyOf(other), orch+"    revocationList: '"+list+"'\n"), "spec.host.revocationList"},
		{"a list with no key", host("", orch+"    revocationList: '"+list+"'\n"), "spec.host.revocationList"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.doc))
			require.Error(t, err)
			var fe *config.FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)
		})
	}
}
