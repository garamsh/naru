package linux

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// newRoot is where LeaveRootfs builds the root it moves to.
const newRoot = "/newroot"

// LeaveRootfs moves the root of the machine from the initramfs's rootfs to a
// tmpfs holding a copy of it, as busybox's switch_root does, and frees the
// rootfs. The kernel refuses pivot_root(2) when the process's root is the
// rootfs, and mvmd's supervisor pivots into a root of its own for every VMM
// (mvmd daemon/internal/supervisor/root.go), so a service could not start a
// Machine from the rootfs itself. It must run first, as PID 1, with nothing
// else mounted.
func LeaveRootfs() error {
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", newRoot, "tmpfs", 0, "mode=0755"); err != nil {
		return fmt.Errorf("mount the new root: %w", err)
	}
	if err := moveContents("/", newRoot, filepath.Base(newRoot)); err != nil {
		return err
	}
	if err := os.Chdir(newRoot); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move the new root over /: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot into the new root: %w", err)
	}
	return os.Chdir("/")
}

// moveContents copies each entry of the directory from into the directory to
// and then removes it from from, except the entry named keep.
func moveContents(from, to, keep string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	links := map[inode]string{}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		src := filepath.Join(from, e.Name())
		if err := copyTree(src, filepath.Join(to, e.Name()), links); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
		if err := os.RemoveAll(src); err != nil {
			return fmt.Errorf("remove %s: %w", src, err)
		}
	}
	return nil
}

// inode names a file on the source file system; links maps the ones with several names to the
// first copy made, so the others become hard links to it.
type inode struct{ dev, ino uint64 }

// copyTree copies the file, directory or symbolic link at src to dst with its
// mode (the set-user-ID, set-group-ID and sticky bits among it), owner and extended attributes. A
// file with several names is copied once and linked under the others, as long as links is shared
// between the calls. Device nodes are recreated; sockets and FIFOs are skipped.
func copyTree(src, dst string, links map[inode]string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(dest, target); err != nil {
				return err
			}
		case mode.IsRegular():
			if st != nil && st.Nlink > 1 {
				key := inode{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev is uint64 or uint32 by architecture
				if first, ok := links[key]; ok {
					return os.Link(first, target)
				}
				links[key] = target
			}
			if err := copyFileMode(p, target, mode.Perm()); err != nil {
				return err
			}
		case mode&fs.ModeDevice != 0 && st != nil:
			kind := uint32(syscall.S_IFBLK)
			if mode&fs.ModeCharDevice != 0 {
				kind = syscall.S_IFCHR
			}
			if err := syscall.Mknod(target, kind|uint32(mode.Perm()), int(st.Rdev)); err != nil { //nolint:unconvert // Rdev is uint64 or uint32 by architecture
				return err
			}
		default:
			return nil
		}
		if st == nil {
			return nil
		}
		if err := os.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
		if mode&fs.ModeSymlink != 0 {
			return nil
		}
		// A change of owner clears the set-user-ID and set-group-ID bits, so the mode is set after it.
		if err := os.Chmod(target, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
			return err
		}
		return copyXattrs(p, target)
	})
}

// copyXattrs sets on dst the extended attributes src has. A file system that has none (ENOTSUP)
// is not an error: there is nothing to keep on it.
func copyXattrs(src, dst string) error {
	size, err := syscall.Listxattr(src, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return fmt.Errorf("list the extended attributes of %s: %w", src, err)
	}
	if size == 0 {
		return nil
	}
	names := make([]byte, size)
	if size, err = syscall.Listxattr(src, names); err != nil {
		return fmt.Errorf("list the extended attributes of %s: %w", src, err)
	}
	for _, name := range strings.Split(string(names[:size]), "\x00") {
		if name == "" {
			continue
		}
		value := make([]byte, 64<<10)
		n, err := syscall.Getxattr(src, name, value)
		if err != nil {
			return fmt.Errorf("read %s of %s: %w", name, src, err)
		}
		if err := syscall.Setxattr(dst, name, value[:n], 0); err != nil {
			return fmt.Errorf("set %s on %s: %w", name, dst, err)
		}
	}
	return nil
}

func copyFileMode(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
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
	return os.Chmod(to, perm)
}

// SealRoot remounts / read-only, so that nothing writes to the system the
// image delivered. It must run after every mount point exists.
func (h *Host) SealRoot() error {
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount / read-only: %w", err)
	}
	return nil
}
