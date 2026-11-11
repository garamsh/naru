// Package release writes the assets a deployment of naru Nodes is made from: the boot disk, one
// data disk that carries the test inputs and nothing of any Node, the guest manifest (the names
// and SHA-256 of the guest kernels and initrds the boot disk carries), an example NoCloud
// user-data per Node for Proxmox, and the SHA256SUMS of them. `make release` runs it.
package release

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// Nodes is the number of Nodes the example user-data is for.
const Nodes = 2

// Proxmox is the network of Node index (1 or 2) in the example deployment: the Nodes' addresses
// on the LAN, a MAC for each card, the gateway and resolvers Proxmox's network-config gives, and a
// Machine prefix of 10.200.<index>.0/24. Each Node's stand-in dials the other Node's address.
func Proxmox(index int) node.Network {
	addrs := [Nodes]string{"192.168.0.40", "192.168.0.61"}
	macs := [Nodes]string{"02:00:00:00:03:E9", "02:00:00:00:03:F4"}
	return node.Network{
		IP:            addrs[index-1],
		Peer:          addrs[index%Nodes],
		MAC:           macs[index-1],
		Netmask:       "255.255.255.0",
		Gateway:       "192.168.0.1",
		Nameservers:   []string{"1.1.1.1", "8.8.8.8"},
		MachinePrefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 200, byte(index), 0}), 24),
	}
}

// Config is the configuration of Node index in the example deployment. Node 1 is the
// control-plane Node and runs the probe.
func Config(index int, inputs fixture.Inputs) node.Config {
	return node.Config{
		Name: fmt.Sprintf("node-%d", index), Index: index, Total: Nodes,
		ControlPlane: index == 1, Probe: index == 1, Inputs: inputs, Network: Proxmox(index),
	}
}

// UserData returns the example user-data files by name: naru-node-<index>.yaml for each Node.
func UserData(inputs fixture.Inputs) (map[string][]byte, error) {
	out := map[string][]byte{}
	for i := 1; i <= Nodes; i++ {
		b, err := Config(i, inputs).NaruYAML()
		if err != nil {
			return nil, err
		}
		out[fmt.Sprintf("naru-node-%d.yaml", i)] = b
	}
	return out, nil
}

// ServiceBinaries returns the binary of every service in a naru.yaml, by service name.
func ServiceBinaries(naruYAML []byte) (map[string]string, error) {
	var n struct {
		Spec struct {
			Services []struct{ Name, Binary string }
		}
	}
	if err := yaml.Unmarshal(naruYAML, &n); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range n.Spec.Services {
		out[s.Name] = s.Binary
	}
	return out, nil
}

// MissingBinaries returns the services of a naru.yaml whose binary is nowhere it will be found:
// one under the data disk's inputs must be in dataDisk, as a path in the inputs tree, and any
// other must be a path the initramfs list installs.
func MissingBinaries(naruYAML []byte, onDataDisk func(guestPath string) bool, initramfsList string) ([]string, error) {
	bins, err := ServiceBinaries(naruYAML)
	if err != nil {
		return nil, err
	}
	var missing []string
	for name, bin := range bins {
		switch {
		case strings.HasPrefix(bin, fixture.GuestRoot+"/"):
			if !onDataDisk(bin) {
				missing = append(missing, fmt.Sprintf("%s: %s is not on the data disk", name, bin))
			}
		case !regexp.MustCompile(`(?m)^file ` + regexp.QuoteMeta(bin) + ` `).MatchString(initramfsList):
			missing = append(missing, fmt.Sprintf("%s: %s is neither on the data disk nor in the initramfs list", name, bin))
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// Options say where Build reads and writes.
type Options struct {
	// Root is the repository root; ImageDir and GuestDir (its guest/ directory) are what make
	// image wrote, and DiskDir what make disk wrote. Out receives the assets.
	Root, ImageDir, DiskDir, GuestDir, Out string
	// Version names the assets: naru-<Version>.qcow2.
	Version string
}

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Asset is a file Build wrote.
type Asset struct {
	Name   string
	Bytes  int64
	SHA256 string
}

// Build writes the assets into o.Out and returns them in the order SHA256SUMS lists them.
func Build(o Options) ([]Asset, error) {
	if !versionPattern.MatchString(o.Version) {
		return nil, fmt.Errorf("version %q is not a name to put in a file name", o.Version)
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "naru15-release-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	inputs, err := fixture.Stage(fixture.Sources{Root: o.Root, ImageDir: o.ImageDir, GuestDir: o.GuestDir}, filepath.Join(work, "inputs"))
	if err != nil {
		return nil, fmt.Errorf("stage the inputs: %w", err)
	}
	if err := fixTimes(inputs.Dir); err != nil {
		return nil, err
	}
	raw, err := node.BuildReleaseDataDisk(work, inputs.Dir)
	if err != nil {
		return nil, err
	}

	var names []string
	boot := "naru-" + o.Version + ".qcow2"
	if err := copyFile(filepath.Join(o.DiskDir, "naru.qcow2"), filepath.Join(o.Out, boot)); err != nil {
		return nil, fmt.Errorf("the boot disk (make disk): %w", err)
	}
	names = append(names, boot)

	data := "naru-data-" + o.Version + ".qcow2"
	if out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", raw, filepath.Join(o.Out, data)).CombinedOutput(); err != nil { //nolint:gosec // paths built here
		return nil, fmt.Errorf("qemu-img convert: %w: %s", err, out)
	}
	names = append(names, data)

	// The guest kernels and initrds are inside the boot disk's initramfs; this lists their names
	// and hashes where a deployer can read them without unpacking it (ADR 0010).
	manifest := "naru-guest-" + o.Version + ".json"
	if err := copyFile(filepath.Join(o.GuestDir, "guest.json"), filepath.Join(o.Out, manifest)); err != nil {
		return nil, fmt.Errorf("the guest manifest (make image): %w", err)
	}
	names = append(names, manifest)

	files, err := UserData(inputs)
	if err != nil {
		return nil, err
	}
	list, err := os.ReadFile(filepath.Join(o.Root, "image/initramfs/initramfs.list"))
	if err != nil {
		return nil, err
	}
	var yamls []string
	for name := range files {
		yamls = append(yamls, name)
	}
	sort.Strings(yamls)
	for _, name := range yamls {
		if err := os.WriteFile(filepath.Join(o.Out, name), files[name], 0o644); err != nil { //nolint:gosec // an example for people to read
			return nil, err
		}
		missing, err := MissingBinaries(files[name], func(p string) bool { return onDisk(raw, p) }, string(list))
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s: %s", name, strings.Join(missing, "; "))
		}
		names = append(names, name)
	}

	var assets []Asset
	var sums strings.Builder
	for _, name := range names {
		a, err := digest(filepath.Join(o.Out, name))
		if err != nil {
			return nil, err
		}
		a.Name = name
		assets = append(assets, a)
		fmt.Fprintf(&sums, "%s  %s\n", a.SHA256, name)
	}
	if err := os.WriteFile(filepath.Join(o.Out, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil { //nolint:gosec // published with the assets
		return nil, err
	}
	return assets, nil
}

// onDisk reports whether the data disk image holds guestPath, a path as the Node sees it.
func onDisk(image, guestPath string) bool {
	rel := strings.TrimPrefix(guestPath, fixture.DataMount)
	out, err := exec.Command("/sbin/debugfs", "-R", "stat "+path.Clean(rel), image).CombinedOutput() //nolint:gosec // arguments built here
	return err == nil && strings.Contains(string(out), "Type: regular")
}

// fixTimes sets the modification time of every file and directory of dir to one fixed moment.
func fixTimes(dir string) error {
	t := time.Unix(1767225600, 0)
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, t, t)
	})
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func digest(p string) (Asset, error) {
	f, err := os.Open(p)
	if err != nil {
		return Asset{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Asset{}, err
	}
	if n == 0 {
		return Asset{}, errors.New(p + " is empty")
	}
	return Asset{Bytes: n, SHA256: fmt.Sprintf("%x", h.Sum(nil))}, nil
}
