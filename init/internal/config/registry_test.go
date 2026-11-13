package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/pkitest"
)

const awsBlock = `  aws:
    region: ap-northeast-2
    trustAnchorArn: arn:aws:rolesanywhere:ap-northeast-2:1:trust-anchor/a
    profileArn: arn:aws:rolesanywhere:ap-northeast-2:1:profile/b
    roleArn: arn:aws:iam::1:role/r
    certificate: /var/lib/naru/gaon-node-agent/node-agent.pem
    key: /var/lib/naru/gaon-node-agent/node-agent.pem
`

func TestParse_AcceptsRegistriesAndAWS(t *testing.T) {
	ca := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	doc := valid + "  registries:\n" +
		"    - host: r.example:5000\n      ca: |\n" + pkitest.Indent(ca.Cert, 8) +
		"      credential: {username: u, password: p}\n" +
		"    - host: 1.dkr.ecr.ap-northeast-2.amazonaws.com\n      credential: {helper: ecr-login}\n" + awsBlock

	cfg, err := config.Parse([]byte(doc))

	require.NoError(t, err)
	require.Len(t, cfg.Spec.Registries, 2)
	assert.Equal(t, "p", cfg.Spec.Registries[0].Credential.Password)
	assert.Equal(t, "ecr-login", cfg.Spec.Registries[1].Credential.Helper)
	assert.Equal(t, "ap-northeast-2", cfg.Spec.AWS.Region)
	_, ok := cfg.Spec.Registry("r.example:5000")
	assert.True(t, ok)
}

func TestParse_RefusesARegistryOrAWSAndNamesTheField(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		field string
	}{
		{"both a helper and a password", "  registries:\n    - host: r.example\n      credential: {helper: ecr-login, username: u, password: p}\n", "spec.registries[0].credential"},
		{"a username with no password", "  registries:\n    - host: r.example\n      credential: {username: u}\n", "spec.registries[0].credential"},
		{"a bad host", "  registries:\n    - host: \"a/b\"\n", "spec.registries[0].host"},
		{"a host twice", "  registries:\n    - host: r.example\n    - host: r.example\n", "spec.registries[1].host"},
		{"a ca that is not PEM", "  registries:\n    - host: r.example\n      ca: nope\n", "spec.registries[0].ca"},
		{"a helper with a slash", "  registries:\n    - host: r.example\n      credential: {helper: ../x}\n", "spec.registries[0].credential.helper"},
		{"an aws with no region", "  aws:\n    trustAnchorArn: arn:a\n    profileArn: arn:b\n    roleArn: arn:c\n    certificate: /a\n    key: /a\n", "spec.aws.region"},
		{"an aws certificate that is relative", "  aws:\n    region: r\n    trustAnchorArn: arn:a\n    profileArn: arn:b\n    roleArn: arn:c\n    certificate: a\n    key: /a\n", "spec.aws.certificate"},
		{"an aws arn that is not one", "  aws:\n    region: r\n    trustAnchorArn: x\n    profileArn: arn:b\n    roleArn: arn:c\n    certificate: /a\n    key: /a\n", "spec.aws.trustAnchorArn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(valid + tt.extra))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
}

func TestParse_ServiceKeptPair(t *testing.T) {
	tests := []struct {
		name, kept, field string
	}{
		{"a relative file", "{file: x.pem, organization: o}", "spec.services[0].keptPair.file"},
		{"no organization", "{file: /x.pem}", "spec.services[0].keptPair.organization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := replaceFirst(valid, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      keptPair: "+tt.kept+"\n")

			_, err := config.Parse([]byte(doc))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
	cfg, err := config.Parse([]byte(replaceFirst(valid, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      keptPair: {file: /x.pem, organization: o}\n")))
	require.NoError(t, err)
	assert.Equal(t, &config.KeptPair{File: "/x.pem", Organization: "o"}, cfg.Spec.Services[0].KeptPair)
}

func TestParse_ServiceWatchedFiles(t *testing.T) {
	tests := []struct {
		name, key, paths, field string
	}{
		{"a relative restartOnChange file", "restartOnChange", "[x.pem]", "spec.services[0].restartOnChange[0]"},
		{"an unclean logFiles file", "logFiles", "[/a, /b/../c]", "spec.services[0].logFiles[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := replaceFirst(valid, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      "+tt.key+": "+tt.paths+"\n")

			_, err := config.Parse([]byte(doc))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), tt.field)
		})
	}
	cfg, err := config.Parse([]byte(replaceFirst(valid, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      restartOnChange: [/x.pem]\n      logFiles: [/l.log]\n")))
	require.NoError(t, err)
	assert.Equal(t, []string{"/x.pem"}, cfg.Spec.Services[0].RestartOnChange)
	assert.Equal(t, []string{"/l.log"}, cfg.Spec.Services[0].LogFiles)
	assert.False(t, cfg.Spec.Services[0].Equal(config.Service{Name: cfg.Spec.Services[0].Name, Binary: cfg.Spec.Services[0].Binary}))
}

func replaceFirst(s, from, to string) string {
	for i := 0; i+len(from) <= len(s); i++ {
		if s[i:i+len(from)] == from {
			return s[:i] + to + s[i+len(from):]
		}
	}
	panic("no " + from)
}
