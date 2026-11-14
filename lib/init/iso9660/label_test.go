package iso9660_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/iso9660"
)

// volume returns a device whose sector 16 starts with a descriptor of the type
// and version given and the label padded with spaces.
func volume(typ byte, version byte, label string) *bytes.Reader {
	dev := make([]byte, 20*2048)
	d := dev[16*2048:]
	d[0] = typ
	copy(d[1:], "CD001")
	d[6] = version
	copy(d[8:40], bytes.Repeat([]byte{' '}, 32))
	copy(d[40:72], bytes.Repeat([]byte{' '}, 32))
	copy(d[40:], label)
	return bytes.NewReader(dev)
}

func TestReadLabel_ReturnsTheVolumeIdentifierWithoutPadding(t *testing.T) {
	label, err := iso9660.ReadLabel(volume(1, 1, "cidata"))

	require.NoError(t, err)
	assert.Equal(t, "cidata", label)
}

func TestReadLabel_RefusesADeviceThatIsNotISO9660(t *testing.T) {
	tests := map[string]*bytes.Reader{
		"zeroes":                      bytes.NewReader(make([]byte, 20*2048)),
		"shorter than sector 16":      bytes.NewReader(make([]byte, 4096)),
		"a supplementary descriptor":  volume(2, 1, "cidata"),
		"an unknown descriptor level": volume(1, 2, "cidata"),
	}
	for name, dev := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := iso9660.ReadLabel(dev)

			assert.ErrorIs(t, err, iso9660.ErrNotISO9660)
		})
	}
}
