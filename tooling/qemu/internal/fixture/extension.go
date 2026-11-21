package fixture

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// The system extension that carries naru-orchd and naru-signer (ADR 0055). make image builds it as
// ExtensionImageName and ExtensionInfoName in the image directory's ExtensionDir; the release
// carries both under ReleaseExtensionName.
const (
	ExtensionName      = "naru-orch"
	ExtensionDir       = "extensions"
	ExtensionImageName = ExtensionName + ".sqfs"
	ExtensionInfoName  = ExtensionName + ".json"
)

// ReleaseExtensionName is the name of the release asset that holds the extension's image
// (".sqfs") or the description of it (".json").
func ReleaseExtensionName(version, suffix string) string {
	return ExtensionName + "-ext-" + version + suffix
}

// ExtensionInfo is what make image writes of the extension it built, and what the release carries
// beside the image: an operator reads the sha256 into spec.extensions.
type ExtensionInfo struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	SHA256   string   `json:"sha256"`
	Services []string `json:"services"`
}

var extensionDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReadExtensionInfo reads and checks the description file at path.
func ReadExtensionInfo(path string) (ExtensionInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ExtensionInfo{}, fmt.Errorf("the extension's description (make image): %w", err)
	}
	var i ExtensionInfo
	if err := json.Unmarshal(b, &i); err != nil {
		return ExtensionInfo{}, fmt.Errorf("%s: %w", path, err)
	}
	if i.Name != ExtensionName || i.Version == "" || len(i.Services) == 0 || !extensionDigest.MatchString(i.SHA256) {
		return ExtensionInfo{}, fmt.Errorf("%s names no %s extension with a version, a SHA-256 and services", path, ExtensionName)
	}
	return i, nil
}

// Extension is the staged system extension: what a Node's data disk carries so that a Node whose
// spec.extensions names it finds it stored (ADR 0055).
type Extension struct {
	ExtensionInfo
	// Image is the squashfs file on the host.
	Image string
	// StoreDir is a directory on the host laid out as a Node's extension store, which becomes
	// /var/lib/naru/extensions on the data disk: <sha256>.sqfs and its manifest <sha256>.json.
	StoreDir string
}

// stageExtension reads the extension make image built in imageDir and lays out its store in
// storeDir.
func stageExtension(imageDir, storeDir string) (Extension, error) {
	info, err := ReadExtensionInfo(filepath.Join(imageDir, ExtensionDir, ExtensionInfoName))
	if err != nil {
		return Extension{}, err
	}
	return writeStore(info, filepath.Join(imageDir, ExtensionDir, ExtensionImageName), storeDir)
}

// writeStore copies image into storeDir under its digest and writes the manifest the Node's
// extension store keeps beside it.
func writeStore(info ExtensionInfo, image, storeDir string) (Extension, error) {
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return Extension{}, err
	}
	manifest, err := json.Marshal(struct {
		Name     string   `json:"name"`
		Version  string   `json:"version"`
		Services []string `json:"services"`
	}{info.Name, info.Version, info.Services})
	if err != nil {
		return Extension{}, err
	}
	stored := filepath.Join(storeDir, info.SHA256+".sqfs")
	if err := copyFile(image, stored); err != nil {
		return Extension{}, fmt.Errorf("the extension's image (make image): %w", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, info.SHA256+".json"), manifest, 0o644); err != nil {
		return Extension{}, err
	}
	return Extension{ExtensionInfo: info, Image: image, StoreDir: storeDir}, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// The system extension that carries the volume add-on (ADR 0083, 0084): make image builds it beside
// naru-orch's, and it runs no service. Stage and ReadRelease store it in the same extension store,
// so every Node's data disk has it for the unpinned entry the layout names on a Kubernetes Node.
const VolumesExtensionName = "naru-volumes"

// VolumesExtensionAssets are the files of the volume extension in a directory: its image and its
// description, as make image writes them (the image directory's ExtensionDir).
func VolumesExtensionAssets(dir string) (image, info string) {
	return filepath.Join(dir, VolumesExtensionName+".sqfs"), filepath.Join(dir, VolumesExtensionName+".json")
}

// ReleaseVolumesExtensionName is ReleaseExtensionName for the volume extension.
func ReleaseVolumesExtensionName(version, suffix string) string {
	return VolumesExtensionName + "-ext-" + version + suffix
}

// ReadVolumesExtensionInfo reads and checks the volume extension's description at path: it names
// naru-volumes, has a version and the SHA-256 of its image, and lists no service.
func ReadVolumesExtensionInfo(path string) (ExtensionInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ExtensionInfo{}, fmt.Errorf("the volume extension's description (make image): %w", err)
	}
	var i ExtensionInfo
	if err := json.Unmarshal(b, &i); err != nil {
		return ExtensionInfo{}, fmt.Errorf("%s: %w", path, err)
	}
	if i.Name != VolumesExtensionName || i.Version == "" || len(i.Services) != 0 || !extensionDigest.MatchString(i.SHA256) {
		return ExtensionInfo{}, fmt.Errorf("%s names no %s extension with a version, a SHA-256 and no service", path, VolumesExtensionName)
	}
	i.Services = []string{}
	return i, nil
}

// storeVolumes lays the volume extension out in storeDir, the way writeStore does naru-orch's, after
// checking that the image is the one its description states.
func storeVolumes(infoPath, image, storeDir string) error {
	info, err := ReadVolumesExtensionInfo(infoPath)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(image)
	if err != nil {
		return fmt.Errorf("the volume extension's image (make image): %w", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != info.SHA256 {
		return fmt.Errorf("%s states SHA-256 %s, and its image is %s (make image)", infoPath, info.SHA256, got)
	}
	_, err = writeStore(info, image, storeDir)
	return err
}
