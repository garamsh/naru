package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/config"
)

const (
	joinSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	joinKey = "naru-ek-fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

const joinForm = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
spec:
  join:
    orch:
      url: https://orch.example.test:7500
      caSHA256: ` + joinSHA + `
    enrollKey: ` + joinKey + `
`

func TestParse_JoinFormDerivesTheDataDiskTheKeyFileAndHostdInJoinMode(t *testing.T) {
	cfg, err := config.Parse([]byte(joinForm))
	require.NoError(t, err)

	assert.Empty(t, cfg.Metadata.Name, "the name is optional in the join form")
	assert.Equal(t, config.DataDisk{Label: "NARUDATA", MountPoint: "/var/lib/naru", FormatBlank: true, FirstBlank: true}, cfg.Spec.DataDisk)
	require.Len(t, cfg.Spec.Files, 1)
	key := cfg.Spec.Files[0]
	assert.Equal(t, "/run/naru-hostd/enroll.key", key.Path)
	assert.Equal(t, "0600", key.Mode)
	assert.Equal(t, joinKey, key.Content)
	assert.Nil(t, key.Owner, "hostd runs as root, whose file this is")
	require.Len(t, cfg.Spec.Services, 1)
	svc := cfg.Spec.Services[0]
	assert.Equal(t, "naru-hostd", svc.Name)
	assert.Equal(t, "/bin/naru-hostd", svc.Binary)
	assert.Equal(t, []string{
		"--join",
		"--orch-url=https://orch.example.test:7500",
		"--orch-ca-sha256=" + joinSHA,
		"--enroll-key=/run/naru-hostd/enroll.key",
		"--host-key=/var/lib/naru/hostd/host.key",
		"--join-config=/var/lib/naru/join/config.yaml",
	}, svc.Args)
	assert.Equal(t, config.RestartOnFailure, svc.RestartPolicy, "no restart after exit 0")
}

func TestParse_JoinFormTakesAName(t *testing.T) {
	cfg, err := config.Parse([]byte(strings.Replace(joinForm, "kind: NodeConfig\n", "kind: NodeConfig\nmetadata:\n  name: edge-1\n", 1)))
	require.NoError(t, err)
	assert.Equal(t, "edge-1", cfg.Metadata.Name)

	_, err = config.Parse([]byte(strings.Replace(joinForm, "kind: NodeConfig\n", "kind: NodeConfig\nmetadata:\n  name: Edge_1\n", 1)))
	assert.Equal(t, []string{"metadata.name"}, fields(t, err))
}

func TestParse_JoinFormParsesAgainAsStored(t *testing.T) {
	first, err := config.Parse([]byte(joinForm))
	require.NoError(t, err)
	dumped, err := yaml.Dump(first)
	require.NoError(t, err)

	again, err := config.Parse(dumped)

	require.NoError(t, err)
	assert.Equal(t, first.Spec.Services, again.Spec.Services, "the store holds the derived form, and loads it as it was")
	assert.Equal(t, first.Spec.Files, again.Spec.Files)
	assert.Equal(t, first.Spec.DataDisk, again.Spec.DataDisk)
	assert.Equal(t, first.Spec.Join, again.Spec.Join)
}

func TestParse_JoinFormRefuses(t *testing.T) {
	tests := []struct{ name, from, to, field string }{
		{"a URL that is not https", "https://orch.example.test:7500", "http://orch.example.test:7500", "spec.join.orch.url"},
		{"a URL without a port", "https://orch.example.test:7500", "https://orch.example.test", "spec.join.orch.url"},
		{"a short CA digest", joinSHA, joinSHA[:62], "spec.join.orch.caSHA256"},
		{"a CA digest that is not hex", joinSHA, strings.Repeat("g", 64), "spec.join.orch.caSHA256"},
		{"a key without the prefix", joinKey, strings.TrimPrefix(joinKey, "naru-"), "spec.join.enrollKey"},
		{"a key of the wrong length", joinKey, joinKey[:len(joinKey)-2], "spec.join.enrollKey"},
		{"a key in upper case", joinKey, "naru-ek-" + strings.ToUpper(joinKey[8:]), "spec.join.enrollKey"},
		{"the wrong kind", "kind: NodeConfig", "kind: Other", "kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(strings.Replace(joinForm, tt.from, tt.to, 1)))

			require.Error(t, err)
			assert.Equal(t, []string{tt.field}, fields(t, err))
		})
	}
}

func TestParse_JoinFormRefusesEveryOtherFieldOfSpecAndNamesIt(t *testing.T) {
	others := map[string]string{
		"services":     "  services:\n    - name: x\n      binary: /bin/x\n",
		"controlPlane": "  controlPlane: true\n",
		"dataDisk":     "  dataDisk:\n    label: OTHER\n    mountPoint: /var/lib/other\n",
		"network":      "  network:\n    forwarding: [ipv4]\n",
		"files":        "  files:\n    - path: /run/x\n      content: y\n",
		"time":         "  time:\n    servers: [10.0.0.1]\n",
		"host":         "  host:\n    region: eu-1\n",
		"orch":         "  orch:\n    peerURL: https://a:1\n",
		"signer":       "  signer:\n    peerListen: 0.0.0.0:1\n",
		"metrics":      "  metrics:\n    listen: 127.0.0.1\n",
		"storage":      "  storage:\n    - name: s\n      label: s\n",
	}
	_, err := config.Parse([]byte(joinForm))
	require.NoError(t, err, "control: the form alone is accepted")
	for name, field := range others {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(joinForm + field))

			require.Error(t, err)
			assert.Contains(t, fields(t, err), "spec."+name)
		})
	}
}

func TestParse_ANodeConfigurationWithSpecJoinIsRefused(t *testing.T) {
	_, err := config.Parse([]byte(valid))
	require.NoError(t, err, "control: the configuration is valid without spec.join")

	_, err = config.Parse([]byte(valid + "  join:\n    orch:\n      url: https://orch.example.test:7500\n      caSHA256: " + joinSHA + "\n    enrollKey: " + joinKey + "\n"))

	require.Error(t, err)
	assert.Contains(t, fields(t, err), "spec.services")
	assert.Contains(t, fields(t, err), "spec.dataDisk")
}

func TestParse_RestartPolicyIsAlwaysOrOnFailure(t *testing.T) {
	for policy, ok := range map[string]bool{"always": true, "on-failure": true, "never": false} {
		t.Run(policy, func(t *testing.T) {
			doc := strings.Replace(valid, "      binary: /usr/bin/mvmd\n", "      binary: /usr/bin/mvmd\n      restartPolicy: "+policy+"\n", 1)

			_, err := config.Parse([]byte(doc))

			if ok {
				require.NoError(t, err)
				return
			}
			assert.Equal(t, []string{"spec.services[0].restartPolicy"}, fields(t, err))
		})
	}
}
