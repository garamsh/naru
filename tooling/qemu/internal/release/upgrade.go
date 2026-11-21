package release

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// The members of the upgrade bundle (ADR 0020): the kernel, which carries the initramfs, and the
// manifest that names its version and digest. A bundle is a tar of exactly these two, in this
// order, owned by root, with every time zero, so the same inputs give the same bytes.
const (
	BundleKernel   = "kernel.efi"
	BundleManifest = "manifest.json"
)

// BundleExtension is an extension image the bundle carries after the manifest (ADR 0084).
type BundleExtension struct {
	// Name is the extension's name and Path the image file.
	Name, Path string
}

// BundleName is the name of the upgrade bundle asset of a release.
func BundleName(version string) string { return "naru-host-upgrade-" + version + ".tar" }

// BundleManifestJSON is the manifest of an upgrade bundle.
type BundleManifestJSON struct {
	Version string `json:"version"`
	Kernel  struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"kernel"`
	// Extensions are the members that follow the manifest, in order.
	Extensions []bundleExtensionJSON `json:"extensions,omitempty"`
	// ESP is the partition of the boot disk this release's image is built with (ADR 0037). A
	// manifest of an earlier release has none.
	ESP struct {
		Bytes int64 `json:"bytes"`
	} `json:"esp"`
}

type bundleExtensionJSON struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// WriteBundle writes the upgrade bundle of version, which holds the kernel at kernelPath, to out.
// espBytes is the size of the ESP of the boot disk built with that kernel. exts are the extension
// images the bundle carries as members extensions/<name>.sqfs after the manifest, which lists them.
func WriteBundle(out, version, kernelPath string, espBytes int64, exts ...BundleExtension) error {
	k, err := os.Open(kernelPath)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, k)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s is empty", kernelPath)
	}
	var m BundleManifestJSON
	m.Version = version
	m.Kernel.Name, m.Kernel.Bytes, m.Kernel.SHA256 = BundleKernel, n, fmt.Sprintf("%x", h.Sum(nil))
	m.ESP.Bytes = espBytes
	for _, e := range exts {
		x, err := os.Open(e.Path)
		if err != nil {
			return err
		}
		eh := sha256.New()
		en, err := io.Copy(eh, x)
		_ = x.Close()
		if err != nil {
			return err
		}
		m.Extensions = append(m.Extensions, bundleExtensionJSON{Name: e.Name, Bytes: en, SHA256: fmt.Sprintf("%x", eh.Sum(nil))})
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	err = writeMember(tw, BundleKernel, n, func() error {
		if _, err := k.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := io.Copy(tw, k)
		return err
	})
	if err == nil {
		err = writeMember(tw, BundleManifest, int64(len(manifest)), func() error { _, err := tw.Write(manifest); return err })
	}
	for i := 0; err == nil && i < len(exts); i++ {
		err = writeMember(tw, "extensions/"+exts[i].Name+".sqfs", m.Extensions[i].Bytes, func() error {
			x, err := os.Open(exts[i].Path)
			if err != nil {
				return err
			}
			defer func() { _ = x.Close() }()
			_, err = io.Copy(tw, x)
			return err
		})
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeMember(tw *tar.Writer, name string, size int64, body func() error) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	return body()
}
