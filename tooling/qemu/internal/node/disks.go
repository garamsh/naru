// Package node builds and boots naru Nodes under QEMU. It needs no
// privilege: the disks are made by mke2fs from a directory (-d), the Nodes
// share a network through QEMU's multicast socket backend, and each runs
// under the caller's KVM access.
package node

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Label and size of the data disk of a Node (docs/architecture/config.md).
const (
	DataLabel = "NARUDATA"

	// The data disk is sparse. It holds the staged inputs, the Machines' root disks and mvmd's state.
	dataDiskBytes = 2 << 30
	mkfs          = "/sbin/mkfs.ext4"
)

// BuildDataDisk writes the data disk into dir and returns its path: ext4 with
// the verity feature, labelled DataLabel, holding at inputs the tree of the
// directory inputsDir (hard-linked into the build, so it costs no copy when
// both are on one file system). It has nothing of the Node in it: what differs
// between Nodes is on the NoCloud drive.
func BuildDataDisk(dir, inputsDir string) (string, error) {
	data := filepath.Join(dir, "data.img")
	trees := map[string]string{}
	if inputsDir != "" {
		trees["inputs"] = inputsDir
	}
	if err := makeDisk(data, DataLabel, dataDiskBytes, []string{"-O", "verity"}, trees, nil); err != nil {
		return "", err
	}
	return data, nil
}

// The fixed values that make the release's data disk a function of its inputs: mke2fs takes the
// file system's UUID, the seed of its directory hashes and the clock from these, not from the
// host.
const (
	releaseUUID      = "6e617275-6461-7461-8000-000000000001"
	releaseHashSeed  = "6e617275-6461-7461-8000-000000000002"
	releaseClockUnix = "1767225600" // 2026-01-01T00:00:00Z
)

// BuildReleaseDataDisk is BuildDataDisk with the UUID, directory hash seed and clock fixed, so
// that the same inputs give the same file system. The mtimes of the inputs must be fixed by the
// caller; the ctimes mke2fs copies from the host are not.
func BuildReleaseDataDisk(dir, inputsDir string) (string, error) {
	data := filepath.Join(dir, "data.img")
	extra := []string{"-O", "verity", "-U", releaseUUID, "-E", "hash_seed=" + releaseHashSeed}
	env := []string{"E2FSPROGS_FAKE_TIME=" + releaseClockUnix, "SOURCE_DATE_EPOCH=" + releaseClockUnix}
	if err := makeDisk(data, DataLabel, dataDiskBytes, extra, map[string]string{"inputs": inputsDir}, env); err != nil {
		return "", err
	}
	return data, nil
}

// DataDiskBytes is the virtual size of the data disk.
const DataDiskBytes = dataDiskBytes

// makeDisk creates an ext4 image of size bytes holding the trees (destination
// to host directory), owned by root, with extra mke2fs options. The block size
// is 4096 so that verity, which needs the file system block to equal the page
// size, can be enabled.
func makeDisk(image, label string, size int64, extra []string, trees map[string]string, env []string) error {
	src := image + ".src"
	if err := os.RemoveAll(src); err != nil {
		return err
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		return err
	}
	for dest, from := range trees {
		if err := linkTree(from, filepath.Join(src, dest)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(image, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(image, size); err != nil {
		return err
	}
	args := append([]string{"-q", "-F", "-b", "4096", "-L", label, "-E", "root_owner=0:0", "-d", src}, extra...)
	args = append(args, image, strconv.FormatInt(size/1024, 10)+"k")
	cmd := exec.Command(mkfs, args...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", mkfs, args, err, out)
	}
	return os.RemoveAll(src)
}

// linkTree recreates the tree of from at to, hard-linking each file and
// copying it when a link is refused. Directories are 0755.
func linkTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.Link(p, target); err == nil {
			return nil
		}
		return copyInto(p, target)
	})
}

func copyInto(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
