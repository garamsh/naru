package linux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// movingPrefix starts the name an entry has in the storage target while it is being copied there.
const movingPrefix = ".naru-moving-"

// moveEntry moves the entry src of a directory of mvmd's state to dst in the storage target, which
// is on another file system. The copy is made under a temporary name, fsynced, and renamed to dst;
// only then is src removed. An interruption leaves the entry whole in src, or in both places with
// the same content, and the next boot finishes it: a copy left under the temporary name is dropped
// and made again, and a dst that equals src is the finished copy, so src is removed. A dst that
// differs from src is a name conflict, and the move is refused.
func moveEntry(src, dst string, log *slog.Logger) error {
	if _, err := os.Lstat(dst); err == nil {
		same, err := sameTree(src, dst)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%s and %s both exist and differ; neither is overwritten, and mvmd will refuse to start with its disks, snapshots and disk snapshots on two file systems", src, dst)
		}
		if err := os.RemoveAll(src); err != nil {
			return err
		}
		log.Info("storage entry already copied; the source is removed", "from", src, "to", dst)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), movingPrefix+filepath.Base(dst))
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := copyTree(src, tmp, map[inode]string{}); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, tmp, err)
	}
	if err := syncTree(tmp); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	if err := syncPath(filepath.Dir(dst)); err != nil {
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		return err
	}
	log.Info("storage entry moved from mvmd's state directory", "from", src, "to", dst)
	return nil
}

// syncTree fsyncs every file and directory under root, root included.
func syncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || !(d.IsDir() || d.Type().IsRegular()) {
			return nil
		}
		return syncPath(p)
	})
}

// syncPath fsyncs the file or directory at p.
func syncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sameTree reports whether a and b are the same file, directory or link with the same content.
func sameTree(a, b string) (bool, error) {
	ia, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Lstat(b)
	if err != nil {
		return false, err
	}
	if ia.Mode().Type() != ib.Mode().Type() {
		return false, nil
	}
	switch {
	case ia.IsDir():
		ea, err := os.ReadDir(a)
		if err != nil {
			return false, err
		}
		eb, err := os.ReadDir(b)
		if err != nil {
			return false, err
		}
		if len(ea) != len(eb) {
			return false, nil
		}
		for i := range ea {
			if ea[i].Name() != eb[i].Name() {
				return false, nil
			}
			if same, err := sameTree(filepath.Join(a, ea[i].Name()), filepath.Join(b, eb[i].Name())); err != nil || !same {
				return false, err
			}
		}
		return true, nil
	case ia.Mode()&fs.ModeSymlink != 0:
		la, err := os.Readlink(a)
		if err != nil {
			return false, err
		}
		lb, err := os.Readlink(b)
		return la == lb, err
	case ia.Mode().IsRegular():
		if ia.Size() != ib.Size() {
			return false, nil
		}
		return sameContent(a, b)
	}
	return true, nil
}

// sameContent reports whether the files a and b hold the same bytes.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer func() { _ = fb.Close() }()
	bufA, bufB := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if errA != nil || errB != nil {
			if (errA == io.EOF || errA == io.ErrUnexpectedEOF) && (errB == io.EOF || errB == io.ErrUnexpectedEOF) {
				return true, nil
			}
			return false, errors.Join(errA, errB)
		}
	}
}
