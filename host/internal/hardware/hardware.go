// Package hardware reads what a host is made of, for the report a host that booted from the install
// seed sends orch with its enrollment: its processors, memory, disks and network links. The person
// who admits the host reads it beside the pairing code, to know which machine is asking.
package hardware

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/garamsh/naru/lib/orchapi"
)

// blankProbe is how much of the start of a disk is read to tell whether it is blank: past a partition
// table, an ext4 or XFS superblock and btrfs's, which sits at 64 KiB.
const blankProbe = 128 << 10

// Source is where the report is read from. Live is the running system; a test points the paths at a
// tree it built.
type Source struct {
	// Meminfo is /proc/meminfo, Sys /sys and Dev /dev.
	Meminfo, Sys, Dev string
	CPUs              int
}

// Live is the running system.
func Live() Source {
	return Source{Meminfo: "/proc/meminfo", Sys: "/sys", Dev: "/dev", CPUs: runtime.NumCPU()}
}

// Collect reads the report. A disk it cannot read to the end is reported as not blank: an operator
// is never told a disk is free when it is not known to be.
func (s Source) Collect() (*orchapi.Hardware, error) {
	mem, err := s.memory()
	if err != nil {
		return nil, err
	}
	disks, err := s.disks()
	if err != nil {
		return nil, err
	}
	links, err := s.links()
	if err != nil {
		return nil, err
	}
	return &orchapi.Hardware{CPUs: s.CPUs, MemoryBytes: mem, Disks: disks, Links: links}, nil
}

func (s Source) memory() (uint64, error) {
	f, err := os.Open(s.Meminfo)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("%s: MemTotal is %q", s.Meminfo, strings.TrimSpace(rest))
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: MemTotal: %w", s.Meminfo, err)
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("%s has no MemTotal", s.Meminfo)
}

// virtual names the block devices that are not disks of the machine: RAM and loop devices, device
// mapper, compressed RAM, optical and floppy drives.
func virtual(name string) bool {
	for _, p := range []string{"loop", "ram", "dm-", "zram", "sr", "fd", "md", "nbd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s Source) disks() ([]orchapi.Disk, error) {
	entries, err := os.ReadDir(filepath.Join(s.Sys, "block"))
	if err != nil {
		return nil, err
	}
	out := []orchapi.Disk{}
	for _, e := range entries {
		name := e.Name()
		if virtual(name) {
			continue
		}
		dir := filepath.Join(s.Sys, "block", name)
		sectors, err := readUint(filepath.Join(dir, "size"))
		if err != nil {
			return nil, err
		}
		out = append(out, orchapi.Disk{
			Name: name, Size: sectors * 512, Model: readText(filepath.Join(dir, "device", "model")),
			Blank: s.blank(name, dir),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// blank is whether the disk has no partition and starts with nothing but zeros.
func (s Source) blank(name, dir string) bool {
	parts, err := filepath.Glob(filepath.Join(dir, name+"*", "partition"))
	if err != nil || len(parts) > 0 {
		return false
	}
	f, err := os.Open(filepath.Join(s.Dev, name))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, blankProbe)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	return n > 0 && len(bytes.Trim(buf[:n], "\x00")) == 0
}

func (s Source) links() ([]orchapi.Link, error) {
	entries, err := os.ReadDir(filepath.Join(s.Sys, "class", "net"))
	if err != nil {
		return nil, err
	}
	out := []orchapi.Link{}
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		dir := filepath.Join(s.Sys, "class", "net", name)
		// carrier cannot be read from an interface that is down, which has none.
		out = append(out, orchapi.Link{Name: name, MAC: readText(filepath.Join(dir, "address")), Carrier: readText(filepath.Join(dir, "carrier")) == "1"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
