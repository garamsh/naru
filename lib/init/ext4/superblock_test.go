package ext4_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/ext4"
	"github.com/garamsh/naru/lib/init/ext4/ext4test"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestReadSuperblock_ReadsTheLabelAndFeatures(t *testing.T) {
	withVerity := open(t, ext4test.Image(t, "NARUDATA", "verity", nil))
	without := open(t, ext4test.Image(t, "PLAIN", "", nil))

	sb, err := ext4.ReadSuperblock(withVerity)
	require.NoError(t, err)
	assert.Equal(t, "NARUDATA", sb.Label)
	assert.True(t, sb.Verity())

	sb, err = ext4.ReadSuperblock(without)
	require.NoError(t, err)
	assert.Equal(t, "PLAIN", sb.Label)
	assert.False(t, sb.Verity(), "the control image must lack the feature the other has")
}

func TestCheckVerity(t *testing.T) {
	tests := []struct {
		name string
		r    func(t *testing.T) *bytes.Reader
		want error
	}{
		{"ext4 with verity", func(t *testing.T) *bytes.Reader { return image(t, "verity") }, nil},
		{"ext4 without verity", func(t *testing.T) *bytes.Reader { return image(t, "") }, ext4.ErrNoVerity},
		{"not ext4", func(*testing.T) *bytes.Reader { return bytes.NewReader(make([]byte, 4096)) }, ext4.ErrNotExt4},
		{"shorter than a superblock", func(*testing.T) *bytes.Reader { return bytes.NewReader([]byte("x")) }, ext4.ErrNotExt4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ext4.CheckVerity(tt.r(t))

			if tt.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

func image(t *testing.T, features string) *bytes.Reader {
	t.Helper()
	data, err := os.ReadFile(ext4test.Image(t, "X", features, nil))
	require.NoError(t, err)
	return bytes.NewReader(data)
}
