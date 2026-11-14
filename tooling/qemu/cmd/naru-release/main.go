// Command naru-release writes the release assets into a directory: the boot disk, the upgrade bundle, the shared data
// disk, the guest manifest, the Node boot set (a kernel, an initramfs and the manifest naming them), a test-only user-data per Node that runs gaon, naru-inputs-<version>.json
// (the Firecracker digest and the test image's reference), the naructl binary, and SHA256SUMS. `make release` runs it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "naru-release:", err)
		os.Exit(1)
	}
}

func run() error {
	var o release.Options
	flag.StringVar(&o.Root, "root", ".", "repository root")
	flag.StringVar(&o.ImageDir, "image", "", "directory make image writes")
	flag.StringVar(&o.DiskDir, "disk", "", "directory make disk writes")
	flag.StringVar(&o.GuestDir, "guest", "", "the guest directory of make image (guest.json)")
	flag.StringVar(&o.Out, "out", "", "directory that receives the assets")
	flag.StringVar(&o.Version, "version", "", "version in the asset names")
	flag.StringVar(&o.Naructl, "naructl", "", "the naructl binary (static, linux/amd64): an asset, and it writes the user-data")
	flag.StringVar(&o.PrevManifest, "prev-manifest", "", "the manifest.json of the previous release's upgrade bundle, to refuse a kernel its Nodes cannot take (ADR 0037)")
	flag.BoolVar(&o.RequiresRecreation, "requires-recreation", false, "allow a kernel the previous release's Nodes cannot take: the notes tell them to be recreated")
	flag.StringVar(&o.Figures, "notes-figures", "", "a file to write the release notes' figures to (ESP_BYTES, LARGEST_UPGRADE_KERNEL_BYTES); not in -out, whose files are all assets")
	flag.Parse()
	if o.ImageDir == "" || o.DiskDir == "" || o.GuestDir == "" || o.Out == "" || o.Version == "" || o.Naructl == "" {
		return fmt.Errorf("-image, -disk, -guest, -out, -version and -naructl are required")
	}
	assets, err := release.Build(o)
	if err != nil {
		return err
	}
	for _, a := range assets {
		fmt.Printf("%s  %s  %d bytes\n", a.SHA256, a.Name, a.Bytes)
	}
	fmt.Printf("release: %d asset(s) and SHA256SUMS in %s; the data disk is %d GiB virtual\n", len(assets), o.Out, node.DataDiskBytes>>30)
	return nil
}
