// Package node builds and boots one naru Node under QEMU. It needs no
// privilege: the disks are made by mke2fs from a directory (-d) and the Node
// runs under the caller's KVM access.
package node

import (
	"embed"
	"fmt"
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
	dataDiskBytes   = 64 << 20
	mkfs            = "/sbin/mkfs.ext4"
)

//go:embed files/naru.yaml files/mvmd.json
var files embed.FS

// NaruYAML is the node configuration the tests boot with: mvmd and the
// stand-in as services, and the data disk labelled DataLabel.
func NaruYAML() []byte { return mustRead("files/naru.yaml") }

// MvmdJSON is mvmd's configuration, which the data disk carries at
// config/mvmd.json and the mvmd service names with -config.
func MvmdJSON() []byte { return mustRead("files/mvmd.json") }

func mustRead(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// BuildDisks writes the config disk and the data disk into dir and returns
// their paths. The config disk is ext4 labelled ConfigLabel holding
// naru.yaml; the data disk is ext4 with the verity feature, labelled
// DataLabel, holding config/mvmd.json.
func BuildDisks(dir string, naruYAML, mvmdJSON []byte) (config, data string, err error) {
	config = filepath.Join(dir, "config.img")
	if err := makeDisk(config, ConfigLabel, configDiskBytes, nil, map[string][]byte{"naru.yaml": naruYAML}); err != nil {
		return "", "", err
	}
	data = filepath.Join(dir, "data.img")
	if err := makeDisk(data, DataLabel, dataDiskBytes, []string{"-O", "verity"}, map[string][]byte{"config/mvmd.json": mvmdJSON}); err != nil {
		return "", "", err
	}
	return config, data, nil
}

// makeDisk creates an ext4 image of size bytes holding files, with extra
// mke2fs options. The block size is 4096 so that verity, which needs the
// file system block to equal the page size, can be enabled.
func makeDisk(image, label string, size int64, extra []string, content map[string][]byte) error {
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
	if err := os.WriteFile(image, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(image, size); err != nil {
		return err
	}
	args := append([]string{"-q", "-F", "-b", "4096", "-L", label, "-d", src}, extra...)
	args = append(args, image, strconv.FormatInt(size/1024, 10)+"k")
	if out, err := exec.Command(mkfs, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", mkfs, args, err, out)
	}
	return os.RemoveAll(src)
}
