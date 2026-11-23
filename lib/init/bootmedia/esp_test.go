package bootmedia_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/bootmedia"
	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/wire"
)

// sysfs lays out /sys/class/block with one disk whose partition has uuid, as the kernel does.
func sysfs(t *testing.T, uuid string) string {
	t.Helper()
	root := t.TempDir()
	class := filepath.Join(root, "class/block")
	require.NoError(t, os.MkdirAll(class, 0o755))
	d := filepath.Join(root, "devices", "vda")
	p := filepath.Join(d, "vda1")
	require.NoError(t, os.MkdirAll(p, 0o755))
	require.NoError(t, os.Symlink(d, filepath.Join(class, "vda")))
	require.NoError(t, os.Symlink(p, filepath.Join(class, "vda1")))
	for name, content := range map[string]string{"uevent": "PARTUUID=" + uuid + "\n", "partition": "1\n", "start": "2048\n", "size": "1024\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(p, name), []byte(content), 0o644))
	}
	return class
}

// esp is an ESP source whose mount writes files into the mount point and whose unmount counts.
type esp struct {
	bootmedia.ESP
	mounted, unmounted int
	device             string
}

func newESP(t *testing.T, uuid string, files map[string]string) *esp {
	t.Helper()
	e := &esp{}
	mp := filepath.Join(t.TempDir(), "esp")
	e.ESP = bootmedia.ESP{
		SysClassBlock: sysfs(t, uuid), DevDir: "/dev", GUID: wire.ESPGUID, Mountpoint: mp,
		Mount: func(source, target, fstype string, flags uintptr, data string) error {
			e.mounted++
			e.device = source
			for name, content := range files {
				path := filepath.Join(target, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			}
			return nil
		},
		Unmount: func(string, int) error { e.unmounted++; return nil },
	}
	return e
}

func index(t *testing.T, entries ...bootmedia.IndexEntry) string {
	t.Helper()
	b, err := json.Marshal(bootmedia.Index{Extensions: entries})
	require.NoError(t, err)
	return string(b)
}

func TestESP_OfferReadsTheImageTheIndexNamesAndUnmountsWhenTheBodyIsClosed(t *testing.T) {
	e := newESP(t, wire.ESPGUID, map[string]string{
		"EXT/INDEX.JSN": index(t, bootmedia.IndexEntry{Name: "naru-volumes", Version: "v1", SHA256: "ab", Bytes: 5, File: "E0.SQF"}),
		"EXT/E0.SQF":    "image",
	})

	off, err := e.Offer("naru-volumes")

	require.NoError(t, err)
	assert.Equal(t, "/dev/vda1", e.device)
	assert.Equal(t, "v1", off.Version)
	assert.Equal(t, "ab", off.SHA256)
	body, err := io.ReadAll(off.Body)
	require.NoError(t, err)
	assert.Equal(t, "image", string(body))
	assert.Zero(t, e.unmounted, "the partition stays mounted while the image is read")
	require.NoError(t, off.Body.Close())
	assert.Equal(t, 1, e.unmounted)
}

func TestESP_OfferHasNothingWhenTheNameIsNotListedOrThereIsNoIndexOrNoPartition(t *testing.T) {
	listed := index(t, bootmedia.IndexEntry{Name: "naru-volumes", Version: "v1", SHA256: "ab", Bytes: 5, File: "E0.SQF"})
	for name, e := range map[string]*esp{
		"another name":   newESP(t, wire.ESPGUID, map[string]string{"EXT/INDEX.JSN": listed, "EXT/E0.SQF": "image"}),
		"no index":       newESP(t, wire.ESPGUID, map[string]string{"EFI/BOOT/BOOTX64.EFI": "k"}),
		"no ESP on disk": newESP(t, "11111111-2222-3333-4444-555555555555", nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.Offer("naru-orch")

			require.ErrorIs(t, err, extension.ErrNoOffer)
			assert.Equal(t, e.mounted, e.unmounted, "every mount is unmounted")
		})
	}
	// The control: the listed name is offered from the same ESP.
	e := newESP(t, wire.ESPGUID, map[string]string{"EXT/INDEX.JSN": listed, "EXT/E0.SQF": "image"})
	off, err := e.Offer("naru-volumes")
	require.NoError(t, err)
	require.NoError(t, off.Body.Close())
}

func TestESP_OfferRefusesAFileNameThatLeavesTheDirectory(t *testing.T) {
	e := newESP(t, wire.ESPGUID, map[string]string{
		"EXT/INDEX.JSN": index(t, bootmedia.IndexEntry{Name: "naru-volumes", Version: "v1", SHA256: "ab", Bytes: 5, File: "../EFI/BOOT/BOOTX64.EFI"}),
	})

	_, err := e.Offer("naru-volumes")

	require.ErrorContains(t, err, "not a file of EXT")
	assert.Equal(t, e.mounted, e.unmounted)
}
