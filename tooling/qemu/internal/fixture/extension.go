package fixture

import (
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
	if err := os.WriteFile(filepath.Join(storeDir, info.SHA256+".json"), manifest, 0o644); err != nil { //nolint:gosec // read through the data disk by init
		return Extension{}, err
	}
	return Extension{ExtensionInfo: info, Image: image, StoreDir: storeDir}, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // paths built here
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // a staged input
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
