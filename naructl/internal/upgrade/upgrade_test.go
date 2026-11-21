package upgrade_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/upgrade"
)

const (
	sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestSumFor_FindsTheLineOfTheFileByItsName(t *testing.T) {
	tests := map[string]struct {
		sums    string
		want    string
		wantErr bool
	}{
		"a text line":                {sumA + "  naru-upgrade-v2.tar\n", sumA, false},
		"a binary line":              {sumA + " *naru-upgrade-v2.tar\n", sumA, false},
		"among others":               {sumB + "  naru.img\n" + sumA + "  naru-upgrade-v2.tar\n" + sumB + "  bzImage\n", sumA, false},
		"a path in the line":         {sumA + "  ./out/naru-upgrade-v2.tar\n", sumA, false},
		"upper case digits":          {strings.ToUpper(sumA) + "  naru-upgrade-v2.tar\n", sumA, false},
		"the same digest twice":      {sumA + "  naru-upgrade-v2.tar\n" + sumA + "  naru-upgrade-v2.tar\n", sumA, false},
		"another file only":          {sumA + "  naru.img\n", "", true},
		"a name that is a suffix":    {sumA + "  xnaru-upgrade-v2.tar\n", "", true},
		"two digests":                {sumA + "  naru-upgrade-v2.tar\n" + sumB + "  naru-upgrade-v2.tar\n", "", true},
		"a digest that is too short": {"abcd  naru-upgrade-v2.tar\n", "", true},
		"a digest that is not hex":   {strings.Repeat("z", 64) + "  naru-upgrade-v2.tar\n", "", true},
		"an empty file":              {"", "", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := upgrade.SumFor(strings.NewReader(tt.sums), "naru-upgrade-v2.tar")

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheck_AcceptsTheFileWithTheDigestAndRefusesAnyOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.tar")
	require.NoError(t, os.WriteFile(path, []byte("abc"), 0o644))
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	require.NoError(t, upgrade.Check(path, abc), "the control: the right digest")
	assert.Error(t, upgrade.Check(path, sumA))
	assert.Error(t, upgrade.Check(path+".none", abc))
}
