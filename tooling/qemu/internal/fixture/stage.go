package fixture

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/garamsh/naru/tooling/qemu/internal/mkfs"
)

// Where the staged tree appears inside a Node: the data disk is mounted at
// /var/lib/mvmd, and the tree is the disk's inputs directory.
const (
	DataMount = "/var/lib/mvmd"
	GuestRoot = DataMount + "/inputs"
)

// Inputs is the staged tree and what the Node's configuration says of it.
type Inputs struct {
	// Dir is the tree on the host; it becomes GuestRoot on every Node's data disk.
	Dir string
	// Image is the OCI image the registry serves.
	Image Image
	// Digests of the files mvmd verifies, by the names of its configuration.
	FirecrackerSHA256, KernelSHA256, GuestInitrdSHA256, ConverterInitrdSHA256 string
}

// Sources are the host files and directories Stage takes its inputs from.
type Sources struct {
	// Root is the repository root, where go.work is.
	Root string
	// ImageDir holds inputs.env and vmlinux, which make image writes.
	ImageDir string
	// GuestDir holds guest-inputs.env and the two initrds, which image/guest/build.sh writes.
	GuestDir string
}

// Stage builds the test programs and assembles the inputs tree in out.
//
//	bin/mkfs.ext4 bin/mke2fs bin/ld.so bin/lib/*   mvmd's mkfs, see internal/mkfs
//	bin/naru-registry bin/naru-probe                services the tests configure
//	guest/vmlinux guest/guest-initrd.cpio guest/converter-initrd.cpio
//	registry/                                       see WriteRegistry
//
// Directories are 0755 and files 0644 or 0755, since the VMMs run as users
// of mvmd's UID range and read the guest files through them.
func Stage(src Sources, out string) (Inputs, error) {
	env, err := readEnv(filepath.Join(src.ImageDir, "inputs.env"))
	if err != nil {
		return Inputs{}, err
	}
	guestEnv, err := readEnv(filepath.Join(src.GuestDir, "guest-inputs.env"))
	if err != nil {
		return Inputs{}, err
	}
	in := Inputs{
		Dir:                   out,
		FirecrackerSHA256:     env["MVMD_FIRECRACKER_SHA256"],
		KernelSHA256:          env["MVMD_KERNEL_SHA256"],
		GuestInitrdSHA256:     guestEnv["MVMD_INITRD_SHA256"],
		ConverterInitrdSHA256: guestEnv["MVMD_CONVERTER_INITRD_SHA256"],
	}
	for name, v := range map[string]string{
		"MVMD_FIRECRACKER_SHA256": in.FirecrackerSHA256, "MVMD_KERNEL_SHA256": in.KernelSHA256,
		"MVMD_INITRD_SHA256": in.GuestInitrdSHA256, "MVMD_CONVERTER_INITRD_SHA256": in.ConverterInitrdSHA256,
	} {
		if v == "" {
			return Inputs{}, fmt.Errorf("%s is not in inputs.env or guest-inputs.env", name)
		}
	}
	bin := filepath.Join(out, "bin")
	if err := os.MkdirAll(filepath.Join(bin, mkfs.LibDir), 0o755); err != nil {
		return Inputs{}, err
	}
	entryDir, err := os.MkdirTemp("", "naru15-entry-")
	if err != nil {
		return Inputs{}, err
	}
	defer func() { _ = os.RemoveAll(entryDir) }()
	for _, b := range []struct{ module, pkg, out string }{
		{"tooling/qemu", "./cmd/naru-registry", filepath.Join(bin, "naru-registry")},
		{"tooling/qemu", "./cmd/naru-mkfs", filepath.Join(bin, "mkfs.ext4")},
		{"tooling/qemu", "./cmd/naru-entry", filepath.Join(entryDir, "entry")},
		{"tooling/probe", "./cmd/naru-probe", filepath.Join(bin, "naru-probe")},
	} {
		if err := goBuild(filepath.Join(src.Root, b.module), b.pkg, b.out); err != nil {
			return Inputs{}, err
		}
	}
	if err := stageMke2fs(bin); err != nil {
		return Inputs{}, err
	}
	guest := filepath.Join(out, "guest")
	if err := os.MkdirAll(guest, 0o755); err != nil {
		return Inputs{}, err
	}
	for from, to := range map[string]string{
		env["MVMD_KERNEL"]: "vmlinux",
		filepath.Join(src.GuestDir, "guest-initrd.cpio"):     "guest-initrd.cpio",
		filepath.Join(src.GuestDir, "converter-initrd.cpio"): "converter-initrd.cpio",
	} {
		if err := copyFile(from, filepath.Join(guest, to), 0o644); err != nil {
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

// stageMke2fs puts the host's mke2fs, its loader and libraries into bin.
func stageMke2fs(bin string) error {
	exe, loader, libs, err := mkfs.HostMke2fs()
	if err != nil {
		return err
	}
	if err := copyFile(exe, filepath.Join(bin, mkfs.Mke2fs), 0o755); err != nil {
		return err
	}
	if err := copyFile(loader, filepath.Join(bin, mkfs.Loader), 0o755); err != nil {
		return err
	}
	for _, l := range libs {
		if err := copyFile(l, filepath.Join(bin, mkfs.LibDir, filepath.Base(l)), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies from to to with mode, following symbolic links.
func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(to, mode)
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
