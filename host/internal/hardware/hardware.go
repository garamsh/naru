// Package hardware reads what a host is made of: for the report a host that booted from the install
// seed sends orch with its enrollment, its processors, memory, disks and network links, which the
// person who admits the host reads beside the pairing code; and for GET /v1/host, whether it runs
// its microVMs on metal or nested.
package hardware

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
)

// Source is where the report is read from. Live is the running system; a test points the paths at a
// tree it built.
type Source struct {
	// Meminfo is /proc/meminfo, Cpuinfo /proc/cpuinfo and Sys /sys.
	Meminfo, Cpuinfo, Sys string
	CPUs                  int
}

// Live is the running system.
func Live() Source {
	return Source{Meminfo: "/proc/meminfo", Cpuinfo: "/proc/cpuinfo", Sys: "/sys", CPUs: runtime.NumCPU()}
}

// Virtualization is how the host runs microVMs, the value of hostapi.LabelVirtualization:
// nested when a processor's flags include hypervisor, which a CPU running under a hypervisor
// reports, and metal otherwise (ADR 0110 item 3).
func (s Source) Virtualization() (string, error) {
	f, err := os.Open(s.Cpuinfo)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(key) == "flags" && slices.Contains(strings.Fields(value), "hypervisor") {
			return hostapi.VirtualizationNested, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", s.Cpuinfo, err)
	}
	return hostapi.VirtualizationMetal, nil
}

// Collect reads the report. It states what it reads and calls no disk blank: that is init's check.
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
			Partitions: partitions(dir, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// partitions counts the partitions the kernel lists under the disk.
func partitions(dir, name string) int {
	parts, err := filepath.Glob(filepath.Join(dir, name+"*", "partition"))
	if err != nil {
		return 0
	}
	return len(parts)
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
