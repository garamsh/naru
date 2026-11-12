package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/pkitest"
)

// pkiDoc returns a configuration whose spec.pki issues one server certificate
// and one client certificate from one authority, with the CA key given.
func pkiDoc(t *testing.T, ca pkitest.CA, key string) string {
	t.Helper()
	doc := `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: apiserver
      binary: /usr/bin/apiserver
    - name: agent
      binary: /usr/bin/agent
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/mvmd
  files:
    - path: /run/other/file
      content: x
  pki:
    authorities:
      - name: ca
        certificate: |
` + pkitest.Indent(ca.Cert, 10)
	if key != "" {
		doc += "        key: |\n" + pkitest.Indent(key, 10)
	}
	return doc + `        certificatePath: /run/pki/ca.pem
    certificates:
      - name: apiserver
        authority: ca
        certificatePath: /run/pki/apiserver.pem
        keyPath: /run/pki/apiserver.key
        commonName: apiserver
        usage: server
        nodeAddresses: true
        validity: 24h
        restart: [apiserver]
      - name: agent
        authority: ca
        certificatePath: /run/pki/agent.pem
        keyPath: /run/pki/agent.key
        commonName: node-1
        organization: nodes
        usage: client
`
}

func newCA(t *testing.T) pkitest.CA {
	t.Helper()
	return pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

func TestParse_AcceptsASpecPKI(t *testing.T) {
	ca := newCA(t)

	cfg, err := config.Parse([]byte(pkiDoc(t, ca, ca.Key)))

	require.NoError(t, err)
	require.Len(t, cfg.Spec.PKI.Authorities, 1)
	require.Len(t, cfg.Spec.PKI.Certificates, 2)
	assert.Equal(t, 24*time.Hour, cfg.Spec.PKI.Certificates[0].ValidFor())
	assert.Equal(t, config.DefaultValidity, cfg.Spec.PKI.Certificates[1].ValidFor(), "a certificate without a validity takes the default")
	assert.Equal(t, []string{"apiserver"}, cfg.Spec.PKI.Certificates[0].Restart)
	_, err = cfg.Spec.PKI.Authorities[0].Signer()
	assert.NoError(t, err)
}

func TestParse_AcceptsTheValidityBounds(t *testing.T) {
	// The control of the two refusals of a validity under an hour and over ten years.
	ca := newCA(t)
	for _, v := range []string{"1h", "87600h"} {
		doc := strings.Replace(pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: "+v, 1)

		cfg, err := config.Parse([]byte(doc))

		require.NoError(t, err, v)
		d, perr := time.ParseDuration(v)
		require.NoError(t, perr)
		assert.Equal(t, d, cfg.Spec.PKI.Certificates[0].ValidFor())
	}
}

func TestParse_AcceptsAnAuthorityKeyPathWhenTheAuthorityCarriesItsKey(t *testing.T) {
	ca := newCA(t)
	doc := strings.Replace(pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/ca.pem\n        keyPath: /run/pki/ca.key", 1)

	cfg, err := config.Parse([]byte(doc))

	require.NoError(t, err)
	assert.Equal(t, "/run/pki/ca.key", cfg.Spec.PKI.Authorities[0].KeyPath)
}

func TestParse_RefusesASpecPKIAndNamesTheField(t *testing.T) {
	ca := newCA(t)
	other := newCA(t)
	tests := []struct {
		name  string
		doc   string
		from  string
		to    string
		field string
	}{
		{"authority without a key", pkiDoc(t, ca, ""), "", "", "spec.pki.certificates[0].authority"},
		{"authority key that does not match", pkiDoc(t, ca, other.Key), "", "", "spec.pki.authorities[0].key"},
		{"authority certificate that is not a CA", pkiDoc(t, ca, ca.Key), pkitest.Indent(ca.Cert, 10), pkitest.Indent(pkitest.NewLeaf(t), 10), "spec.pki.authorities[0].certificate"},
		{"authority certificate that is not PEM", pkiDoc(t, ca, ca.Key), pkitest.Indent(ca.Cert, 10), "          nope\n", "spec.pki.authorities[0].certificate"},
		{"authority keyPath without a key", pkiDoc(t, ca, ""), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/ca.pem\n        keyPath: /run/pki/ca.key", "spec.pki.authorities[0].keyPath"},
		{"authority keyPath outside /run", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/ca.pem\n        keyPath: /etc/ca.key", "spec.pki.authorities[0].keyPath"},
		{"authority keyPath in init's directory", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/ca.pem\n        keyPath: /run/naru/ca.key", "spec.pki.authorities[0].keyPath"},
		{"authority keyPath that is a certificate's path", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/ca.pem\n        keyPath: /run/pki/agent.key", "spec.pki.certificates[1].keyPath"},
		{"certificate path outside /run", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/agent.pem", "certificatePath: /etc/agent.pem", "spec.pki.certificates[1].certificatePath"},
		{"key path in init's directory", pkiDoc(t, ca, ca.Key), "keyPath: /run/pki/agent.key", "keyPath: /run/naru/agent.key", "spec.pki.certificates[1].keyPath"},
		{"certificate path that is init's directory", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/agent.pem", "certificatePath: /run/naru", "spec.pki.certificates[1].certificatePath"},
		{"authority path not clean", pkiDoc(t, ca, ca.Key), "certificatePath: /run/pki/ca.pem", "certificatePath: /run/pki/../ca.pem", "spec.pki.authorities[0].certificatePath"},
		{"path twice in spec.pki", pkiDoc(t, ca, ca.Key), "keyPath: /run/pki/agent.key", "keyPath: /run/pki/apiserver.key", "spec.pki.certificates[1].keyPath"},
		{"path also in spec.files", pkiDoc(t, ca, ca.Key), "keyPath: /run/pki/agent.key", "keyPath: /run/other/file", "spec.pki.certificates[1].keyPath"},
		{"path inside a spec.files file", pkiDoc(t, ca, ca.Key), "keyPath: /run/pki/agent.key", "keyPath: /run/other/file/key", "spec.pki.certificates[1].keyPath"},
		{"certificate that names no authority", pkiDoc(t, ca, ca.Key), "authority: ca\n        certificatePath: /run/pki/agent.pem", "authority: nope\n        certificatePath: /run/pki/agent.pem", "spec.pki.certificates[1].authority"},
		{"restart of an unknown service", pkiDoc(t, ca, ca.Key), "restart: [apiserver]", "restart: [apiserver, nope]", "spec.pki.certificates[0].restart[1]"},
		{"unknown usage", pkiDoc(t, ca, ca.Key), "usage: client", "usage: both", "spec.pki.certificates[1].usage"},
		{"empty common name", pkiDoc(t, ca, ca.Key), "commonName: node-1", "commonName: ''", "spec.pki.certificates[1].commonName"},
		{"duplicate certificate name", pkiDoc(t, ca, ca.Key), "name: agent\n        authority", "name: apiserver\n        authority", "spec.pki.certificates[1].name"},
		{"invalid IP address", pkiDoc(t, ca, ca.Key), "nodeAddresses: true", "ipAddresses: [not-an-ip]", "spec.pki.certificates[0].ipAddresses[0]"},
		{"server without any name", pkiDoc(t, ca, ca.Key), "        nodeAddresses: true\n", "", "spec.pki.certificates[0]"},
		{"validity that is not a duration", pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: soon", "spec.pki.certificates[0].validity"},
		{"validity that is not positive", pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: -1h", "spec.pki.certificates[0].validity"},
		{"validity under an hour", pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: 59m59s", "spec.pki.certificates[0].validity"},
		{"validity over ten years", pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: 87601h", "spec.pki.certificates[0].validity"},
		{"validity that overflows the two thirds of it", pkiDoc(t, ca, ca.Key), "validity: 24h", "validity: 1500000h", "spec.pki.certificates[0].validity"},
		{"unknown field", pkiDoc(t, ca, ca.Key), "usage: client", "usage: client\n        bogus: 1", "spec.pki.certificates[1].bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := tt.doc
			if tt.from != "" {
				require.Contains(t, doc, tt.from, "the case must change the valid document")
				doc = strings.Replace(doc, tt.from, tt.to, 1)
			}

			_, err := config.Parse([]byte(doc))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
}
