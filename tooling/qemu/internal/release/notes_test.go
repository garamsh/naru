package release

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSums_NamesTheByDesignAssetsFromThePatternAndKeepsFileOrder(t *testing.T) {
	var n Notes
	require.NoError(t, n.ReadSums([]byte("bb  naru-host-v1.qcow2\naa  naru-data-v1.qcow2\ncc  naru-data-v1.json\n")))
	assert.Equal(t, 3, n.ListedCount)
	assert.Equal(t, "`naru-data-v1.qcow2`", n.ByDesign, "naru-data-v1.json does not match the pattern")
	assert.Equal(t, []Sum{{"naru-host-v1.qcow2", "bb"}, {"naru-data-v1.qcow2", "aa"}, {"naru-data-v1.json", "cc"}}, n.Sums)
}

func TestReadSums_RefusesWhenNoAssetDiffersByDesign(t *testing.T) {
	var n Notes
	assert.ErrorContains(t, n.ReadSums([]byte("bb  naru-host-v1.qcow2\n")), "no asset matching")
}

func TestRenderNotes_AnUnknownFieldIsAnError(t *testing.T) {
	_, err := RenderNotes("{{.Nope}}", Notes{})
	assert.Error(t, err)
	out, err := RenderNotes("{{.Tag}}{{if .Recreate}}!{{end}}", Notes{Tag: "v1"})
	require.NoError(t, err)
	assert.Equal(t, "v1", string(out))
}

func TestReadCACertDate_ReadsTheDateOfThePinAndRefusesAPinWithoutOne(t *testing.T) {
	var n Notes
	require.NoError(t, n.ReadCACertDate("# a comment\nCACERT_URL=https://curl.se/ca/cacert-2026-11-05.pem\nCACERT_DATE=2026-11-05\n"))
	assert.Equal(t, "2026-11-05", n.CACertDate)
	assert.ErrorContains(t, (&Notes{}).ReadCACertDate("CACERT_URL=x\n"), "no CACERT_DATE")
}
