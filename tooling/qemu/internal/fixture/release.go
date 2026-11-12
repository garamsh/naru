package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/tooling/qemu/internal/guest"
)

// ReleaseInputsName is the name of the release asset that says what the data disk's tree holds: the
// Firecracker digest the Nodes' mvmd configuration names, and the image its registry serves.
func ReleaseInputsName(version string) string { return "naru-inputs-" + version + ".json" }

// ReleaseInputs is the content of that asset.
type ReleaseInputs struct {
	FirecrackerSHA256 string `json:"firecrackerSha256"`
	ImageRepository   string `json:"imageRepository"`
	ImageDigest       string `json:"imageDigest"`
}

// Release is a release directory read: the boot disk, as a raw file, and the inputs tree its data
// disk carries.
type Release struct {
	// Version is the release's version, the part of the asset names after "naru-".
	Version string
	// BootDisk is the boot disk converted to a raw image in the work directory.
	BootDisk string
	// Inputs is the tree extracted from the data disk, in the form Stage returns.
	Inputs Inputs
	// Naructl is the release's naructl-<v>-linux-amd64 asset, copied into the work directory as an
	// executable: it writes the Nodes' user-data.
	Naructl string
}

// ReadRelease reads the assets `make release` writes into dir (naru-<v>.qcow2, naru-data-<v>.qcow2,
// naru-guest-<v>.json and naru-inputs-<v>.json) and prepares what a Node boots from them in work:
// naru.img, the boot disk as a raw file, and inputs/, the tree of the data disk. It needs
// qemu-img and /sbin/debugfs, which the Nodes need as well.
func ReadRelease(dir, work string) (Release, error) {
	guestFile, version, err := releaseVersion(dir)
	if err != nil {
		return Release{}, err
	}
	r := Release{Version: version}
	m, err := guest.Read(guestFile)
	if err != nil {
		return Release{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, ReleaseInputsName(r.Version)))
	if err != nil {
		return Release{}, fmt.Errorf("the release's inputs asset: %w", err)
	}
	var ri ReleaseInputs
	if err := json.Unmarshal(b, &ri); err != nil {
		return Release{}, fmt.Errorf("%s: %w", ReleaseInputsName(r.Version), err)
	}
	if ri.FirecrackerSHA256 == "" || ri.ImageRepository == "" || ri.ImageDigest == "" {
		return Release{}, fmt.Errorf("%s names no Firecracker digest or no image", ReleaseInputsName(r.Version))
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return Release{}, err
	}
	r.BootDisk = filepath.Join(work, "naru.img")
	if err := qemuImgRaw(filepath.Join(dir, "naru-"+r.Version+".qcow2"), r.BootDisk); err != nil {
		return Release{}, err
	}
	data := filepath.Join(work, "data.img")
	if err := qemuImgRaw(filepath.Join(dir, "naru-data-"+r.Version+".qcow2"), data); err != nil {
		return Release{}, err
	}
	defer func() { _ = os.Remove(data) }()
	if out, err := exec.Command("/sbin/debugfs", "-R", "rdump /inputs "+work, data).CombinedOutput(); err != nil { //nolint:gosec // arguments built here
		return Release{}, fmt.Errorf("read the data disk's inputs: %w: %s", err, out)
	}
	r.Inputs = Inputs{
		Dir: filepath.Join(work, "inputs"), FirecrackerSHA256: ri.FirecrackerSHA256, Guest: m,
		Image: Image{Repository: ri.ImageRepository, Digest: ri.ImageDigest},
	}
	if r.Naructl, err = StageNaructl(dir, work); err != nil {
		return Release{}, err
	}
	return r, nil
}

// releaseVersion is the release's guest manifest and version, from the one naru-guest-<v>.json in dir.
func releaseVersion(dir string) (guestFile, version string, err error) {
	guests, err := filepath.Glob(filepath.Join(dir, "naru-guest-*.json"))
	if err != nil || len(guests) != 1 {
		return "", "", fmt.Errorf("%s holds %d naru-guest-<version>.json files, not one", dir, len(guests))
	}
	return guests[0], strings.TrimSuffix(strings.TrimPrefix(filepath.Base(guests[0]), "naru-guest-"), ".json"), nil
}

// StageNaructl copies the release's naructl asset into work as an executable and returns its path.
// It reads nothing else of the release, so naructl can be asked what the release's layout is before
// the disks are converted.
func StageNaructl(dir, work string) (string, error) {
	_, version, err := releaseVersion(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(work, "naructl")
	if err := copyExecutable(filepath.Join(dir, "naructl-"+version+"-linux-amd64"), bin); err != nil {
		return "", fmt.Errorf("the release's naructl asset: %w", err)
	}
	return bin, nil
}

// copyExecutable copies the file from to to, mode 0755: a downloaded asset need not be executable.
func copyExecutable(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o755) //nolint:gosec // an executable asset
}

// qemuImgRaw converts the qcow2 file from to a raw file at to, which stays sparse.
func qemuImgRaw(from, to string) error {
	if out, err := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", from, to).CombinedOutput(); err != nil { //nolint:gosec // paths built here
		return fmt.Errorf("qemu-img convert %s: %w: %s", from, err, out)
	}
	return nil
}
