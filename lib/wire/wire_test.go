package wire_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/wire"
)

func TestIsEnrollKey(t *testing.T) {
	var secret [wire.EnrollKeyBytes]byte
	secret[0], secret[31] = 0xab, 0x0f
	good := wire.EnrollKey(secret)
	tests := map[string]struct {
		key  string
		want bool
	}{
		"a key EnrollKey made":    {good, true},
		"upper-case digits":       {strings.ToUpper(good[len(wire.EnrollKeyPrefix):]), false},
		"upper-case with prefix":  {wire.EnrollKeyPrefix + strings.ToUpper(good[len(wire.EnrollKeyPrefix):]), false},
		"no prefix":               {good[len(wire.EnrollKeyPrefix):], false},
		"one digit short":         {good[:len(good)-1], false},
		"one digit long":          {good + "0", false},
		"a digit that is not hex": {good[:len(good)-1] + "g", false},
		"empty":                   {"", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) { assert.Equal(t, tc.want, wire.IsEnrollKey(tc.key)) })
	}
}

func TestProtocol_ValidIsTheTwoProtocolsInTheirOneSpelling(t *testing.T) {
	for _, p := range wire.Protocols() {
		assert.True(t, p.Valid(), "control: %s is valid", p)
	}
	assert.Equal(t, []wire.Protocol{"TCP", "UDP"}, wire.Protocols())
	for _, p := range []wire.Protocol{"tcp", "udp", "Tcp", "SCTP", ""} {
		assert.False(t, p.Valid(), "%q is not a protocol", p)
	}
}

func TestESPGUID_IsTheOneDiskEnvBuildsTheImageWith(t *testing.T) {
	env, err := os.ReadFile(filepath.Join("..", "..", "image", "host", "disk", "disk.env"))
	require.NoError(t, err)
	var got string
	for _, line := range strings.Split(string(env), "\n") {
		if v, ok := strings.CutPrefix(line, "ESP_GUID="); ok {
			got = v
		}
	}
	require.NotEmpty(t, got, "disk.env has no ESP_GUID line")
	assert.Equal(t, wire.ESPGUID, got)
}
