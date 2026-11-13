package fixture

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/garamsh/naru/tooling/qemu/internal/guest"
)

// Where the staged tree appears inside a Node: the data disk is mounted at
// /var/lib/naru, and the tree is the disk's inputs directory.
const (
	DataMount = "/var/lib/naru"
	GuestRoot = DataMount + "/inputs"
)

// Inputs is the staged tree and what the Node's configuration says of it.
type Inputs struct {
	// Dir is the tree on the host; it becomes GuestRoot on every Node's data disk.
	Dir string
	// Image is the OCI image the registry serves.
	Image Image
	// FirecrackerSHA256 is the digest of the Firecracker binary the image carries.
	FirecrackerSHA256 string
	// Guest names the guest kernels and initrds the image carries, by their paths inside it and
	// their digests, as image/guest/build.sh wrote them (ADR 0010).
	Guest guest.Manifest
}

// Sources are the host files and directories Stage takes its inputs from.
type Sources struct {
	// Root is the repository root, where go.work is.
	Root string
	// ImageDir holds inputs.env, which make image writes.
	ImageDir string
	// GuestDir holds guest.json, which image/guest/build.sh writes.
	GuestDir string
}

// Stage builds the test programs and assembles the inputs tree in out.
//
//	bin/naru-registry bin/naru-probe   services the tests configure
//	bin/naru-bootnext                  (ADR 0020)
//	bin/naru-netecho                   sends, receives and reports routes
//	registry/                          see WriteRegistry
//
// The guest kernel, the initrds and mkfs.ext4 are not staged: the image carries them (ADR 0010,
// ADR 0011).
//
// Directories are 0755 and files 0644 or 0755, since the VMMs run as users
// of mvmd's UID range and read the guest files through them.
func Stage(src Sources, out string) (Inputs, error) {
	env, err := readEnv(filepath.Join(src.ImageDir, "inputs.env"))
	if err != nil {
		return Inputs{}, err
	}
	m, err := guest.Read(filepath.Join(src.GuestDir, "guest.json"))
	if err != nil {
		return Inputs{}, err
	}
	in := Inputs{Dir: out, FirecrackerSHA256: env["MVMD_FIRECRACKER_SHA256"], Guest: m}
	if in.FirecrackerSHA256 == "" {
		return Inputs{}, fmt.Errorf("MVMD_FIRECRACKER_SHA256 is not in inputs.env")
	}
	bin := filepath.Join(out, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return Inputs{}, err
	}
	entryDir, err := os.MkdirTemp("", "naru15-entry-")
	if err != nil {
		return Inputs{}, err
	}
	defer func() { _ = os.RemoveAll(entryDir) }()
	for _, b := range []struct{ module, pkg, out string }{
		{"tooling/qemu", "./cmd/naru-registry", filepath.Join(bin, "naru-registry")},
		{"tooling/qemu", "./cmd/naru-bootnext", filepath.Join(bin, "naru-bootnext")},
		{"tooling/qemu", "./cmd/naru-netecho", filepath.Join(bin, "naru-netecho")},
		{"tooling/qemu", "./cmd/naru-entry", filepath.Join(entryDir, "entry")},
		{"tooling/probe", "./cmd/naru-probe", filepath.Join(bin, "naru-probe")},
	} {
		if err := goBuild(filepath.Join(src.Root, b.module), b.pkg, b.out); err != nil {
			return Inputs{}, err
		}
	}
	entry, err := os.ReadFile(filepath.Join(entryDir, "entry"))
	if err != nil {
		return Inputs{}, err
	}
	in.Image, err = WriteRegistry(filepath.Join(out, "registry"), entry, runtime.GOARCH)
	return in, err
}

// goBuild builds the package of the module as a static Linux executable.
func goBuild(moduleDir, pkg, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, pkg) //nolint:gosec // arguments are built here
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s in %s: %w\n%s", pkg, moduleDir, err, b)
	}
	return nil
}

// readEnv reads KEY=VALUE lines.
func readEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: %q is not KEY=VALUE", path, line)
		}
		env[k] = v
	}
	return env, sc.Err()
}
