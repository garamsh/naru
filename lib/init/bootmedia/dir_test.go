package bootmedia_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/bootmedia"
	"github.com/garamsh/naru/lib/init/extension"
)

func dirWith(t *testing.T, files map[string]string) bootmedia.Dir {
	t.Helper()
	root := filepath.Join(t.TempDir(), "boot-extensions")
	require.NoError(t, os.MkdirAll(root, 0o755))
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
	}
	return bootmedia.Dir{Root: root}
}

func TestDir_OfferReadsTheImageItsDescriptionNamesAndReleaseRemovesIt(t *testing.T) {
	d := dirWith(t, map[string]string{
		"naru-volumes.json": `{"name":"naru-volumes","version":"v1","sha256":"ab","services":[]}`,
		"naru-volumes.sqfs": "image",
	})

	off, err := d.Offer("naru-volumes")

	require.NoError(t, err)
	assert.Equal(t, "ab", off.SHA256)
	assert.Equal(t, "v1", off.Version)
	body, err := io.ReadAll(off.Body)
	require.NoError(t, err)
	assert.Equal(t, "image", string(body))
	require.NoError(t, off.Body.Close())
	require.NoError(t, d.Release())
	_, err = os.Stat(filepath.Join(d.Root, "naru-volumes.sqfs"))
	assert.True(t, os.IsNotExist(err), "the tmpfs copy is freed")
	_, err = d.Offer("naru-volumes")
	assert.ErrorIs(t, err, extension.ErrNoOffer, "and nothing is offered after")
}

func TestDir_OfferHasNothingForAnotherNameOrANameThatLeavesTheDirectoryAndRefusesADescriptionOfAnotherName(t *testing.T) {
	d := dirWith(t, map[string]string{
		"naru-volumes.json": `{"name":"naru-volumes","version":"v1","sha256":"ab"}`,
		"naru-volumes.sqfs": "image",
		"other.json":        `{"name":"naru-volumes","version":"v1","sha256":"ab"}`,
		"other.sqfs":        "image",
	})

	_, err := d.Offer("naru-orch")
	require.ErrorIs(t, err, extension.ErrNoOffer)
	_, err = d.Offer("../boot-extensions/naru-volumes")
	require.ErrorIs(t, err, extension.ErrNoOffer)
	_, err = d.Offer("other")
	require.ErrorContains(t, err, `names "naru-volumes"`)
	// The control: the listed name is offered by the same directory.
	off, err := d.Offer("naru-volumes")
	require.NoError(t, err)
	require.NoError(t, off.Body.Close())
}

func TestDir_OfferHasNothingWhenTheNodeCarriesNoImages(t *testing.T) {
	_, err := bootmedia.Dir{Root: filepath.Join(t.TempDir(), "absent")}.Offer("naru-volumes")

	require.ErrorIs(t, err, extension.ErrNoOffer)
}
