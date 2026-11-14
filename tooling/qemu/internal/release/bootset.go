package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// NodeBootSet is the name of the guest form's Node boot set inside the image: the directory under
// guest/boot-sets/ that holds its kernel.
const NodeBootSet = "naru-node"

// maxBootSetName is the longest name mvmd takes for a boot set.
const maxBootSetName = 63

var notBootSetName = regexp.MustCompile(`[^a-z0-9-]`)

// BootSetName is the name a release's boot set is offered to a host under: naru-node- and the
// version in lower case with every character mvmd's names do not allow (anything but a-z, 0-9 and
// the hyphen) replaced by a hyphen, so v0.4.0 is naru-node-v0-4-0. The manifest's version field
// keeps the version as it is.
func BootSetName(version string) (string, error) {
	name := "naru-node-" + notBootSetName.ReplaceAllString(strings.ToLower(version), "-")
	if len(name) > maxBootSetName {
		return "", fmt.Errorf("version %q makes the boot set name %q, longer than the %d characters mvmd takes", version, name, maxBootSetName)
	}
	return name, nil
}

// BootSetAssets are the names of the three assets of a release's Node boot set: the kernel, the
// initramfs and the manifest that names them.
func BootSetAssets(version string) (kernel, initramfs, manifest string) {
	return "naru-node-kernel-" + version, "naru-node-initramfs-" + version, "naru-node-" + version + ".json"
}

// BootSetFileJSON is a file of a boot set as its manifest records it.
type BootSetFileJSON struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// BootSetJSON is naru-node-<version>.json: the boot set a host is given with `naructl host
// boot-set add`. File names the asset beside the manifest that holds the file.
type BootSetJSON struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Kernel    BootSetFileJSON `json:"kernel"`
	Initramfs BootSetFileJSON `json:"initramfs"`
}

// WriteBootSet copies the kernel and the initramfs into dir under their asset names and writes the
// manifest that names them with their SHA-256.
func WriteBootSet(dir, version, kernelPath, initramfsPath string) error {
	name, err := BootSetName(version)
	if err != nil {
		return err
	}
	kernel, initramfs, manifest := BootSetAssets(version)
	m := BootSetJSON{Name: name, Version: version}
	for _, f := range []struct {
		from, to string
		into     *BootSetFileJSON
	}{{kernelPath, kernel, &m.Kernel}, {initramfsPath, initramfs, &m.Initramfs}} {
		if err := copyFile(f.from, filepath.Join(dir, f.to)); err != nil {
			return err
		}
		a, err := digest(filepath.Join(dir, f.to))
		if err != nil {
			return err
		}
		*f.into = BootSetFileJSON{File: f.to, SHA256: a.SHA256, Bytes: a.Bytes}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifest), append(b, '\n'), 0o644) //nolint:gosec // published with the assets
}

// Figures are the two numbers a release's notes state about its image.
type Figures struct {
	// ESPBytes is the size of the boot disk's ESP.
	ESPBytes int64
	// LargestUpgradeKernel is the largest kernel an in-place upgrade from this release can take.
	LargestUpgradeKernel int64
}

// FiguresOf computes the figures of a release whose kernel is kernelBytes and whose ESP is espBytes.
func FiguresOf(kernelBytes, espBytes int64) Figures {
	var m BundleManifestJSON
	m.Kernel.Bytes, m.ESP.Bytes = kernelBytes, espBytes
	return Figures{ESPBytes: espBytes, LargestUpgradeKernel: m.TrialRoom()}
}

// WriteFigures writes the figures to path as KEY=value lines a workflow step can read.
func WriteFigures(path string, f Figures) error {
	return os.WriteFile(path, fmt.Appendf(nil, "ESP_BYTES=%d\nLARGEST_UPGRADE_KERNEL_BYTES=%d\n", f.ESPBytes, f.LargestUpgradeKernel), 0o644) //nolint:gosec // read by the workflow
}
