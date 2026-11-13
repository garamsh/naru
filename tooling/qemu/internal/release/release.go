// Package release writes the assets a deployment of naru Nodes is made from: the boot disk, the
// upgrade bundle (the kernel with its initramfs, for A/B boot), one
// data disk that carries the test inputs and nothing of any Node, the guest manifest (the names
// and SHA-256 of the guest kernels and initrds the boot disk carries), the inputs file (the Firecracker
// digest and the test image's reference), a test-only NoCloud
// user-data per Node for Proxmox that runs gaon with certificates issued for the release, and the
// SHA256SUMS of them. `make release` runs it.
package release

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// Nodes is the number of Nodes the user-data is for.
const Nodes = 2

// Proxmox is where Node index (1 or 2) is in the example deployment: its address on the LAN and
// the IPv4 prefix mvmd gives its Machines, 10.200.<index>.0/24. Node 1 is the control-plane Node,
// and every agent dials its address.
type Proxmox struct {
	Name, Address, MachinePrefix string
}

// Deployment is the example deployment's Node index.
func Deployment(index int) Proxmox {
	addrs := [Nodes]string{"192.168.0.40/24", "192.168.0.61/24"}
	return Proxmox{
		Name: fmt.Sprintf("node-%d", index), Address: addrs[index-1],
		MachinePrefix: fmt.Sprintf("10.200.%d.0/24", index),
	}
}

// APIServer is the address every Node dials: node-1's.
const APIServer = "192.168.0.40"

// UserDataName is the name of Node index's user-data asset. ".test" marks it as one that carries
// key material, which is for a test deployment only.
func UserDataName(index int) string { return fmt.Sprintf("naru-node-%d.test.yaml", index) }

// UserData has the naructl binary write the user-data files by name (UserDataName), each running
// gaon: mvmd, the registry, etcd and gaon's control plane on node-1, and gaon's Node Agent on both.
// The CA is new at each call and is a test authority: node-1's user-data carries it, private key
// included, in spec.pki, so that its controllers sign the agents' requests (ADR 0016), and every
// Node carries a bootstrap token. The CA's directory is removed before UserData returns, so the key
// is in no file but node-1's user-data, and no file carries a client pair for an operator.
// guest is the guest manifest's path, firecracker the digest of the Firecracker binary, and
// registryCA the path of the authority of the registry service, which every Node trusts for the
// registry's address through spec.registries.
func UserData(tool naructl.Tool, guest, firecracker, registryCA string) (map[string][]byte, error) {
	work, err := os.MkdirTemp("", "naru15-userdata-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	ca := filepath.Join(work, "ca")
	if err := tool.GenCA(ca); err != nil {
		return nil, fmt.Errorf("make the CA: %w", err)
	}
	for i := 1; i <= Nodes; i++ {
		// node-1's user-data, written first, lists the token of every Node.
		if err := tool.GenToken(ca, Deployment(i).Name); err != nil {
			return nil, err
		}
	}
	out := map[string][]byte{}
	for i := 1; i <= Nodes; i++ {
		d := Deployment(i)
		p := filepath.Join(work, UserDataName(i))
		if err := tool.GenNode(naructl.Node{
			CA: ca, Name: d.Name, ControlPlane: i == 1, Address: d.Address, APIServer: APIServer, MachinePrefix: d.MachinePrefix,
			Guest: guest, Firecracker: firecracker, Peers: peers(i), Out: p,
			Registries: []string{"host=" + fixture.RegistryAddr + ",ca=" + registryCA},
		}); err != nil {
			return nil, err
		}
		if out[UserDataName(i)], err = os.ReadFile(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// peers lists the other Nodes of the example deployment as gen node's --peer values: each one's
// address, without its prefix length, and its Machine prefix.
func peers(index int) []string {
	var out []string
	for i := 1; i <= Nodes; i++ {
		if i == index {
			continue
		}
		d := Deployment(i)
		addr, _, _ := strings.Cut(d.Address, "/")
		out = append(out, addr+"="+d.MachinePrefix)
	}
	return out
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
	// Naructl is the naructl binary, built static for linux/amd64: it is an asset, and it writes the
	// user-data.
	Naructl string
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

	// The upgrade bundle is the kernel, initramfs inside, that the boot disk's default slot holds:
	// what an A/B upgrade writes to the trial slot (ADR 0020).
	bundle := BundleName(o.Version)
	if err := WriteBundle(filepath.Join(o.Out, bundle), o.Version, filepath.Join(o.ImageDir, "bzImage")); err != nil {
		return nil, fmt.Errorf("the upgrade bundle (make image): %w", err)
	}
	names = append(names, bundle)

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

	// The data disk's tree is not enough to configure a Node: mvmd's configuration names the
	// Firecracker digest, and a harness needs the image's reference (naru-cluster reads both).
	ri, err := json.MarshalIndent(fixture.ReleaseInputs{
		FirecrackerSHA256: inputs.FirecrackerSHA256, ImageRepository: inputs.Image.Repository, ImageDigest: inputs.Image.Digest,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	inputsAsset := fixture.ReleaseInputsName(o.Version)
	if err := os.WriteFile(filepath.Join(o.Out, inputsAsset), append(ri, '\n'), 0o644); err != nil { //nolint:gosec // published with the assets
		return nil, err
	}
	names = append(names, inputsAsset)

	// naructl is the operator's tool and the one owner of the Node layout: the release ships the
	// binary and writes the test user-data with it.
	tool := "naructl-" + o.Version + "-linux-amd64"
	if err := copyFile(o.Naructl, filepath.Join(o.Out, tool)); err != nil {
		return nil, fmt.Errorf("the naructl binary: %w", err)
	}
	if err := os.Chmod(filepath.Join(o.Out, tool), 0o755); err != nil { //nolint:gosec // an executable asset
		return nil, err
	}
	names = append(names, tool)

	files, err := UserData(naructl.Tool{Bin: o.Naructl}, filepath.Join(o.GuestDir, "guest.json"), inputs.FirecrackerSHA256, filepath.Join(inputs.Dir, "registry", "ca.pem"))
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
		if err := os.WriteFile(filepath.Join(o.Out, name), files[name], 0o600); err != nil {
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
