package release

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	mib         = 1 << 20
	sectorBytes = 512
	// firstEntryOffset is where the first GPT partition entry is: the table starts at LBA 2.
	firstEntryOffset = 2 * sectorBytes
)

// ESPBytes reads the size of the first partition of the raw GPT disk image at path: the ESP.
func ESPBytes(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	entry := make([]byte, 48)
	if _, err := f.ReadAt(entry, firstEntryOffset); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	first, last := binary.LittleEndian.Uint64(entry[32:40]), binary.LittleEndian.Uint64(entry[40:48])
	if first == 0 || last < first {
		return 0, fmt.Errorf("%s has no partition at the first entry of a GPT", path)
	}
	return int64(last-first+1) * sectorBytes, nil
}

// ReadManifest reads the manifest.json of an upgrade bundle that the caller has taken out of it.
func ReadManifest(path string) (BundleManifestJSON, error) {
	var m BundleManifestJSON
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if m.Kernel.Bytes <= 0 {
		return m, fmt.Errorf("%s names no kernel size", path)
	}
	return m, nil
}

// ESPOf is the ESP size of the image a release was built with: the one its manifest records, or,
// for a release before ADR 0037, which records none, the size the disk builder of that time gave
// it: the larger of 64 MiB and two kernels with 8 MiB, a whole number of MiB.
func (m BundleManifestJSON) ESPOf() int64 {
	if m.ESP.Bytes > 0 {
		return m.ESP.Bytes
	}
	return max(64*mib, (2*m.Kernel.Bytes+8*mib+mib-1)/mib*mib)
}

// TrialRoom is the largest kernel the ESP of a Node installed from this release's image can take
// as its trial slot while its default slot holds this release's kernel. The room leaves out the
// FAT32 allocation tables of 512-byte clusters (two of 1/128 of the partition each) with a margin
// of a MiB, which together are esp/64 and a MiB.
func (m BundleManifestJSON) TrialRoom() int64 {
	esp := m.ESPOf()
	return esp - esp/64 - mib - m.Kernel.Bytes
}

// checkPrevious applies CheckSuccessor to the previous release's manifest o names, and says on
// standard output what it did, since a release with no manifest to check against passes silently
// otherwise.
func checkPrevious(o Options, kernelPath string) error {
	if o.PrevManifest == "" {
		fmt.Println("release: no previous manifest given (PREV_MANIFEST): the upgrade from the previous release's image was not checked")
		return nil
	}
	prev, err := ReadManifest(o.PrevManifest)
	if err != nil {
		return fmt.Errorf("the previous release's manifest: %w", err)
	}
	info, err := os.Stat(kernelPath)
	if err != nil {
		return err
	}
	err = CheckSuccessor(prev, info.Size())
	switch {
	case err == nil:
		fmt.Printf("release: the kernel of %d bytes fits the trial slot of a Node installed from %s (room %d bytes)\n", info.Size(), prev.Version, prev.TrialRoom())
		return nil
	case o.RequiresRecreation:
		fmt.Printf("release: %v; the release is marked as requiring recreation, so its notes must say so\n", err)
		return nil
	}
	return fmt.Errorf("%w; mark the release as requiring recreation (RECREATE=1) and say so in its notes, or shrink the kernel", err)
}

// CheckSuccessor returns an error when a kernel of kernelBytes would not fit the trial slot of a
// Node installed from the release prev describes, so that Node cannot take the next bundle.
func CheckSuccessor(prev BundleManifestJSON, kernelBytes int64) error {
	if room := prev.TrialRoom(); kernelBytes > room {
		return fmt.Errorf("the kernel of %d bytes does not fit the trial slot of a Node installed from %s: its ESP of %d bytes holds that release's kernel of %d bytes and %d bytes more",
			kernelBytes, prev.Version, prev.ESPOf(), prev.Kernel.Bytes, room)
	}
	return nil
}
