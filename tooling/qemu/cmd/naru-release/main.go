// Command naru-release writes the release assets into a directory: the boot disk, the upgrade bundle, the shared data
// disk, the guest manifest, the Node boot set (a kernel, an initramfs and the manifest naming them), a test-only user-data per Node that runs gaon, naru-inputs-<version>.json
// (the Firecracker digest and the test image's reference), the naructl and naru-cluster binaries, the SPDX SBOM, the tar of the
// corresponding source of the GPL-family components, and SHA256SUMS. `make release` runs it.
//
// naru-release compare <published SHA256SUMS> <new SHA256SUMS> exits 0 when a tag's published release and a new build of it agree on
// every asset, and 1 naming each asset that differs (the release workflow refuses to touch a release that does).
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

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
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		return compare(os.Args[2:])
	}
	var o release.Options
	flag.StringVar(&o.Root, "root", ".", "repository root")
	flag.StringVar(&o.ImageDir, "image", "", "directory make image writes")
	flag.StringVar(&o.DiskDir, "disk", "", "directory make disk writes")
	flag.StringVar(&o.GuestDir, "guest", "", "the guest directory of make image (guest.json)")
	flag.StringVar(&o.Out, "out", "", "directory that receives the assets")
	flag.StringVar(&o.Version, "version", "", "version in the asset names")
	flag.StringVar(&o.Naructl, "naructl", "", "the naructl binary (static, linux/amd64): an asset, and it writes the user-data")
	flag.StringVar(&o.NaruCluster, "naru-cluster", "", "the naru-cluster binary (static, linux/amd64): an asset")
	flag.StringVar(&o.PrevManifest, "prev-manifest", "", "the manifest.json of the previous release's upgrade bundle, to refuse a kernel its Nodes cannot take (ADR 0037)")
	flag.BoolVar(&o.RequiresRecreation, "requires-recreation", false, "allow a kernel the previous release's Nodes cannot take: the notes tell them to be recreated")
	flag.StringVar(&o.Figures, "notes-figures", "", "a file to write the release notes' figures to (ESP_BYTES, LARGEST_UPGRADE_KERNEL_BYTES); not in -out, whose files are all assets")
	var epoch, binaries, downloads string
	flag.StringVar(&epoch, "epoch", "", "SOURCE_DATE_EPOCH: the only time in the SBOM and the sources tar")
	flag.StringVar(&binaries, "binaries", "", "comma-separated directories whose Go binaries the SBOM states the modules of")
	flag.StringVar(&downloads, "downloads", "", "comma-separated directories that hold the verified upstream tarballs make image fetched")
	flag.StringVar(&o.SourceArchive, "source-archive", "", "the git archive (a tar) of the commit being built")
	flag.Parse()
	var err error
	if o.Epoch, err = strconv.ParseInt(epoch, 10, 64); err != nil {
		return fmt.Errorf("-epoch %q is not a number of seconds: %w", epoch, err)
	}
	o.Binaries, o.Downloads = split(binaries), split(downloads)
	if o.SourceArchive == "" {
		return fmt.Errorf("-source-archive is required (make source-archive)")
	}
	if o.ImageDir == "" || o.DiskDir == "" || o.GuestDir == "" || o.Out == "" || o.Version == "" || o.Naructl == "" || o.NaruCluster == "" {
		return fmt.Errorf("-image, -disk, -guest, -out, -version, -naructl and -naru-cluster are required")
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

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func compare(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: naru-release compare <published SHA256SUMS> <new SHA256SUMS>")
	}
	a, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	b, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	if err := release.Compare(a, b); err != nil {
		return err
	}
	fmt.Println("naru-release: the published release and this build agree on every asset")
	return nil
}
