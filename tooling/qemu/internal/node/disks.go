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

// Labels and sizes of the two disks of a Node (docs/architecture/config.md).
const (
	ConfigLabel = "NARUCFG"
	DataLabel   = "NARUDATA"

	configDiskBytes = 4 << 20
	// The data disk is sparse. It holds the staged inputs, the Machines' root disks and mvmd's state.
	dataDiskBytes = 2 << 30
	mkfs          = "/sbin/mkfs.ext4"
)

// BuildDisks writes the config disk and the data disk into dir and returns
// their paths. The config disk is ext4 labelled ConfigLabel holding
// naru.yaml; the data disk is ext4 with the verity feature, labelled
// DataLabel, holding config/mvmd.json and, at inputs, the tree of the
// directory inputsDir (hard-linked into the build, so it costs no copy when
// both are on one file system).
func BuildDisks(dir string, naruYAML, mvmdJSON []byte, inputsDir string) (config, data string, err error) {
	config = filepath.Join(dir, "config.img")
	if err := makeDisk(config, ConfigLabel, configDiskBytes, nil, map[string][]byte{"naru.yaml": naruYAML}, nil); err != nil {
		return "", "", err
	}
	data = filepath.Join(dir, "data.img")
	trees := map[string]string{}
	if inputsDir != "" {
		trees["inputs"] = inputsDir
	}
	if err := makeDisk(data, DataLabel, dataDiskBytes, []string{"-O", "verity"}, map[string][]byte{"config/mvmd.json": mvmdJSON}, trees); err != nil {
		return "", "", err
	}
	return config, data, nil
}

// makeDisk creates an ext4 image of size bytes holding files and the trees
// (destination to host directory), owned by root, with extra mke2fs options.
// The block size is 4096 so that verity, which needs the file system block to
// equal the page size, can be enabled.
func makeDisk(image, label string, size int64, extra []string, content map[string][]byte, trees map[string]string) error {
	src := image + ".src"
	if err := os.RemoveAll(src); err != nil {
		return err
	}
	for name, b := range content {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
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
	if out, err := exec.Command(mkfs, args...).CombinedOutput(); err != nil {
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
