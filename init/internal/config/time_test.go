package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
)

// timeDoc is the valid document with a spec.time naming servers.
func timeDoc(servers ...string) string {
	if len(servers) == 0 {
		return valid + "  time:\n    servers: []\n"
	}
	doc := valid + "  time:\n    servers:\n"
	for _, s := range servers {
		doc += "      - " + s + "\n"
	}
	return doc
}

func TestParse_AcceptsASpecTimeAndGivesEachServerItsPort(t *testing.T) {
	cfg, err := config.Parse([]byte(timeDoc("ntp.example.net", "time.example.net:1123", "192.0.2.7", "'[2001:db8::1]:124'", "'2001:db8::2'")))

	require.NoError(t, err)
	require.NotNil(t, cfg.Spec.Time)
	assert.Equal(t, []string{"ntp.example.net:123", "time.example.net:1123", "192.0.2.7:123", "[2001:db8::1]:124", "[2001:db8::2]:123"}, cfg.Spec.Time.Addrs())
}

func TestParse_AConfigurationWithoutSpecTimeHasNoServers(t *testing.T) {
	cfg, err := config.Parse([]byte(valid))

	require.NoError(t, err)
	assert.Nil(t, cfg.Spec.Time)
	assert.Empty(t, cfg.Spec.Time.Addrs())
}

func TestParse_RefusesASpecTimeAndNamesTheField(t *testing.T) {
	tests := []struct {
		name  string
		doc   string
		field string
	}{
		{"no server", timeDoc(), "spec.time.servers"},
		{"empty host", timeDoc("'  '"), "spec.time.servers[0]"},
		{"host with a space", timeDoc("'ntp example'"), "spec.time.servers[0]"},
		{"host with a scheme", timeDoc("'udp://ntp.example'"), "spec.time.servers[0]"},
		{"host that starts with a hyphen", timeDoc("-ntp.example"), "spec.time.servers[0]"},
		{"port zero", timeDoc("ntp.example:0"), "spec.time.servers[0]"},
		{"port too large", timeDoc("ntp.example:65536"), "spec.time.servers[0]"},
		{"port that is a word", timeDoc("ntp.example:ntp"), "spec.time.servers[0]"},
		{"empty port", timeDoc("'ntp.example:'"), "spec.time.servers[0]"},
		{"the second of two is bad", timeDoc("ntp.example", "bad host"), "spec.time.servers[1]"},
		{"the same server twice", timeDoc("ntp.example", "ntp.example:123"), "spec.time.servers[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.doc))

			require.Error(t, err)
			assert.Equal(t, []string{tt.field}, fields(t, err))
		})
	}
}

func TestParse_RefusesAnUnknownSpecTimeField(t *testing.T) {
	_, err := config.Parse([]byte(valid + "  time:\n    servers: [ntp.example]\n    bogus: 1\n"))

	require.Error(t, err)
}
