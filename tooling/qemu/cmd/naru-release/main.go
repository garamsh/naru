// Command naru-release writes the release assets into a directory: the boot disk, the upgrade bundle, the shared data
// disk, the guest manifest, the Node boot set (a kernel, an initramfs and the manifest naming them), a test-only user-data per Node that runs gaon, naru-inputs-<version>.json
// (the Firecracker digest and the test image's reference), the naructl and naru-cluster binaries, the SPDX SBOM, the tar of the
// corresponding source of the GPL-family components, and SHA256SUMS. `make release` runs it.
//
// naru-release compare <published SHA256SUMS> <new SHA256SUMS> exits 0 when a tag's published release and a new build of it agree on
// every asset, and 1 naming each asset that differs (the release workflow refuses to touch a release that does).
//
// naru-release previous <tag> reads release tags, one per line, from standard input and prints the one with the highest
// SemVer 2.0.0 version below <tag>, or nothing when none is below it.
//
// naru-release commit [-root <root>] <component> prints the commit of the component a pin.env under image/ names, resolved
// from the Go module of its <PREFIX>_COMMIT_FROM when the pin holds none. The image build takes mvmd's commit from it, and
// so does the SBOM, so the two read one resolver.
//
// naru-release notes renders the release notes template with the tag, the commit, the two figures and the build's SHA256SUMS
// (see -h for the flags) to standard output.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "compare":
			return compare(os.Args[2:])
		case "previous":
			return previous(os.Args[2:], os.Stdin, os.Stdout)
		case "notes":
			return notes(os.Args[2:], os.Stdout)
		case "commit":
			return commit(os.Args[2:], os.Stdout)
		}
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
	var epoch, binaries, archives, downloads string
	flag.StringVar(&epoch, "epoch", "", "SOURCE_DATE_EPOCH: the only time in the SBOM and the sources tar")
	flag.StringVar(&binaries, "binaries", "", "comma-separated directories whose Go binaries the SBOM states the modules of")
	flag.StringVar(&archives, "archives", "", "comma-separated gzip-compressed cpio archives (the initramfs) whose Go binaries the SBOM states the modules of")
	flag.StringVar(&downloads, "downloads", "", "comma-separated directories that hold the verified upstream tarballs make image fetched")
	flag.StringVar(&o.SourceArchive, "source-archive", "", "the git archive (a tar) of the commit being built")
	flag.Parse()
	var err error
	if o.Epoch, err = strconv.ParseInt(epoch, 10, 64); err != nil {
		return fmt.Errorf("-epoch %q is not a number of seconds: %w", epoch, err)
	}
	o.Binaries, o.Archives, o.Downloads = split(binaries), split(archives), split(downloads)
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

func previous(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: naru-release previous <tag> < tags")
	}
	var tags []string
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			tags = append(tags, t)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	prev, err := release.Previous(args[0], tags)
	if err != nil {
		return err
	}
	if prev != "" {
		fmt.Fprintln(out, prev)
	}
	return nil
}

func commit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: naru-release commit [-root <root>] <component>")
	}
	comps, err := release.ReadComponents(*root)
	if err != nil {
		return err
	}
	for _, c := range comps {
		if c.Name == fs.Arg(0) {
			if c.Commit == "" {
				return fmt.Errorf("commit: %s is not pinned by a commit", c.Name)
			}
			_, err := fmt.Fprintln(out, c.Commit)
			return err
		}
	}
	return fmt.Errorf("commit: no pin.env under %s/image names a component %q", *root, fs.Arg(0))
}

func notes(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	var n release.Notes
	var tmpl, sums, cacertPin string
	fs.StringVar(&cacertPin, "cacert-pin", "", "image/pkgs/cacert/pin.env, whose CACERT_DATE the notes state")
	fs.StringVar(&tmpl, "template", "", "the release notes template (docs/operate/release-notes.md.tmpl)")
	fs.StringVar(&sums, "sums", "", "the build's SHA256SUMS")
	fs.StringVar(&n.Tag, "tag", "", "the release's tag")
	fs.StringVar(&n.Commit, "commit", "", "the commit built")
	fs.Int64Var(&n.ESPBytes, "esp-bytes", 0, "the boot disk's ESP, in bytes")
	fs.Int64Var(&n.UpgradeKernelBytes, "upgrade-kernel-bytes", 0, "the largest kernel an in-place upgrade can take, in bytes")
	fs.BoolVar(&n.Recreate, "requires-recreation", false, "tell Nodes of an earlier image to be recreated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if tmpl == "" || sums == "" || n.Tag == "" || n.Commit == "" || n.ESPBytes <= 0 || n.UpgradeKernelBytes <= 0 || cacertPin == "" {
		return fmt.Errorf("notes: -template, -sums, -tag, -commit, -esp-bytes, -upgrade-kernel-bytes and -cacert-pin are required")
	}
	text, err := os.ReadFile(tmpl)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	if err := n.ReadSums(b); err != nil {
		return err
	}
	pin, err := os.ReadFile(cacertPin)
	if err != nil {
		return err
	}
	if err := n.ReadCACertDate(string(pin)); err != nil {
		return err
	}
	rendered, err := release.RenderNotes(string(text), n)
	if err != nil {
		return err
	}
	_, err = out.Write(rendered)
	return err
}
