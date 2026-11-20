package upgrade_test

import (
	"archive/tar"
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

func TestBundleVersion_ReadsTheVersionOfTheManifestAndRefusesABundleWithoutOne(t *testing.T) {
	write := func(members map[string]string) string {
		path := filepath.Join(t.TempDir(), "b.tar")
		f, err := os.Create(path)
		require.NoError(t, err)
		tw := tar.NewWriter(f)
		for _, name := range []string{"kernel.efi", "manifest.json"} {
			body, ok := members[name]
			if !ok {
				continue
			}
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
			_, err = tw.Write([]byte(body))
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())
		require.NoError(t, f.Close())
		return path
	}

	got, err := upgrade.BundleVersion(write(map[string]string{"kernel.efi": "k", "manifest.json": `{"version":"v2"}`}))
	require.NoError(t, err)
	assert.Equal(t, "v2", got)
	for name, members := range map[string]map[string]string{
		"no manifest":        {"kernel.efi": "k"},
		"no version":         {"kernel.efi": "k", "manifest.json": `{}`},
		"a manifest of junk": {"kernel.efi": "k", "manifest.json": `x`},
	} {
		_, err := upgrade.BundleVersion(write(members))
		assert.Error(t, err, name)
	}
	_, err = upgrade.BundleVersion(filepath.Join(t.TempDir(), "none.tar"))
	assert.Error(t, err)
}

func TestMissingExtensions_NamesTheExtensionsWithNoStoredImageForTheVersion(t *testing.T) {
	old := upgrade.Image{Name: "naru-orch", SHA256: sumA, Version: "v1"}
	next := upgrade.Image{Name: "naru-orch", SHA256: sumB, Version: "v2"}
	entries := func(shas ...string) (e []upgrade.Entry) {
		for _, s := range shas {
			e = append(e, upgrade.Entry{Name: "naru-orch", SHA256: s})
		}
		return e
	}

	assert.Equal(t, []string{"naru-orch"}, upgrade.MissingExtensions(entries(sumA), []upgrade.Image{old, next}, "v2"), "the new image is stored but not named")
	assert.Equal(t, []string{"naru-orch"}, upgrade.MissingExtensions(entries(sumA, sumB), []upgrade.Image{old}, "v2"), "the new entry's image is not stored")
	assert.Empty(t, upgrade.MissingExtensions(entries(sumA, sumB), []upgrade.Image{old, next}, "v2"), "the control: both are named and stored")
	assert.Empty(t, upgrade.MissingExtensions(nil, nil, "v2"))
}
