package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/pkitest"
)

// apiDoc is the valid document with spec.api appended. The serving pair is a
// certificate authority's, which is a certificate and key that match.
func apiDoc(t *testing.T, listen, clientCA, cert, key string) string {
	t.Helper()
	doc := valid + "  api:\n"
	if listen != "" {
		doc += "    listen: " + listen + "\n"
	}
	doc += "    clientCA: |\n" + pkitest.Indent(clientCA+"\n", 6)
	doc += "    certificate: |\n" + pkitest.Indent(cert+"\n", 6)
	doc += "    key: |\n" + pkitest.Indent(key+"\n", 6)
	return doc
}

func TestParse_AcceptsASpecAPI(t *testing.T) {
	ca, server := newCA(t), newCA(t)

	cfg, err := config.Parse([]byte(apiDoc(t, "127.0.0.1:50001", ca.Cert, server.Cert, server.Key)))

	require.NoError(t, err)
	require.NotNil(t, cfg.Spec.API)
	assert.Equal(t, "127.0.0.1:50001", cfg.Spec.API.ListenAddr())
	assert.Equal(t, ca.Cert, cfg.Spec.API.ClientCA)
}

func TestParse_SpecAPIListensOnPort50000ByDefault(t *testing.T) {
	ca := newCA(t)

	cfg, err := config.Parse([]byte(apiDoc(t, "", ca.Cert, ca.Cert, ca.Key)))

	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:50000", cfg.Spec.API.ListenAddr())
}

func TestParse_RefusesASpecAPIAndNamesTheField(t *testing.T) {
	ca, other := newCA(t), newCA(t)
	leaf := pkitest.NewLeaf(t)
	tests := []struct {
		name                        string
		listen, clientCA, cert, key string
		field                       string
	}{
		{"listen without a port", "127.0.0.1", ca.Cert, ca.Cert, ca.Key, "spec.api.listen"},
		{"listen port zero", "127.0.0.1:0", ca.Cert, ca.Cert, ca.Key, "spec.api.listen"},
		{"listen is a name", "localhost:50000", ca.Cert, ca.Cert, ca.Key, "spec.api.listen"},
		{"client CA is not PEM", "", "nope", ca.Cert, ca.Key, "spec.api.clientCA"},
		{"client CA is not a CA", "", leaf, ca.Cert, ca.Key, "spec.api.clientCA"},
		{"certificate is not PEM", "", ca.Cert, "nope", ca.Key, "spec.api.certificate"},
		{"key is not PEM", "", ca.Cert, ca.Cert, "nope", "spec.api.key"},
		{"key does not match", "", ca.Cert, ca.Cert, other.Key, "spec.api.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(apiDoc(t, tt.listen, tt.clientCA, tt.cert, tt.key)))

			require.Error(t, err)
			assert.Equal(t, []string{tt.field}, fields(t, err))
		})
	}
}

func TestParse_RefusesAnUnknownSpecAPIField(t *testing.T) {
	ca := newCA(t)
	doc := strings.Replace(apiDoc(t, "", ca.Cert, ca.Cert, ca.Key), "  api:\n", "  api:\n    bogus: 1\n", 1)

	_, err := config.Parse([]byte(doc))

	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.api.bogus")
}

func TestParse_FileRestart(t *testing.T) {
	with := func(restart string) string {
		return strings.Replace(valid, "      content: secret\n", "      content: secret\n      restart: "+restart+"\n", 1)
	}

	cfg, err := config.Parse([]byte(with("[mvmd, etcd]")))
	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd", "etcd"}, cfg.Spec.Files[1].Restart)

	// The control.
	_, err = config.Parse([]byte(with("[ghost]")))
	require.Error(t, err)
	assert.Equal(t, []string{"spec.files[1].restart[0]"}, fields(t, err))
}
