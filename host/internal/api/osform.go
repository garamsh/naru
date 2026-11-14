package api

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/iso9660"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// The OS form: a microVM with no image and no root disk, booted from a boot set the node offers,
// whose first boot is configured by a NoCloud seed (docs/architecture/host.md §OS form).
const (
	// seedLabel is the volume label init looks for.
	seedLabel = "cidata"
	// maxSeedBytes is mvmd's bound on the contents of a disk made with them.
	maxSeedBytes = 1 << 20
	// maxCmdline is mvmd's bound on the kernel command line a client adds.
	maxCmdline = 2048
	// The names of the OS form's disks, after the microVM's own.
	dataDiskName = "data"
	seedDiskName = "seed"
)

// isOSForm reports whether the request carries a field only the OS form has.
func isOSForm(r hostapi.CreateRequest) bool {
	return r.BootSet != "" || r.Cmdline != "" || len(r.Seed) > 0 || r.DataDisk > 0 || r.PodPrefix != ""
}

// validRoutedPrefix reports whether mvmd would route the prefix to a Machine: an IPv4 prefix of /16
// to /32, with no host bits.
func validRoutedPrefix(s string) bool {
	p, err := netip.ParsePrefix(s)
	return err == nil && p == p.Masked() && p.Addr().Is4() && p.Bits() >= 16
}

// validateOS returns the plan for an OS-form request, or every field that is wrong with it.
func validateOS(r hostapi.CreateRequest) (plan, []string, string) {
	var fields []string
	bad := func(f string) { fields = append(fields, f) }
	// The image form's fields, which the OS form has no use for.
	if r.Image != "" {
		bad("image")
	}
	if r.RootDiskBytes != 0 {
		bad("rootDiskBytes")
	}
	if len(r.Disks) > 0 {
		bad("disks")
	}
	if len(r.Env) > 0 {
		bad("env")
	}
	if len(r.Command) > 0 {
		bad("command")
	}
	if len(fields) > 0 {
		return plan{}, fields, "a microVM has an image or a boot set, not both: bootSet, cmdline, seed, dataDisk and podPrefix are the OS form's, image, rootDiskBytes, disks, env and command the image form's"
	}
	if !dnsLabel.MatchString(r.Name) {
		bad("name")
	}
	if r.BootSet == "" {
		bad("bootSet")
	}
	if len(r.Cmdline) > maxCmdline || strings.ContainsAny(r.Cmdline, "\x00\n") {
		bad("cmdline")
	}
	if r.CPU == 0 {
		bad("cpu")
	}
	if r.Memory == 0 {
		bad("memory")
	}
	if r.DataDisk == 0 {
		bad("dataDisk")
	}
	var routed []string
	if r.PodPrefix != "" {
		if !validRoutedPrefix(r.PodPrefix) {
			bad("podPrefix")
		} else {
			routed = []string{r.PodPrefix}
		}
	}
	fields = append(fields, checkRules("networkPolicy.egress", r.NetworkPolicy.Egress)...)
	fields = append(fields, checkRules("networkPolicy.ingress", r.NetworkPolicy.Ingress)...)
	// The volume mvmd is given is the seed's, not the user-data's: it must fit mvmd's bound. The
	// address and gateway are not known until mvmd has made the Machine, so the longest stand in.
	if len(r.Seed) == 0 {
		bad("seed")
	} else if v, err := seedVolume(r.Name, r.Seed, netip.MustParsePrefix("255.255.255.255/32"), netip.MustParseAddr("255.255.255.255")); err != nil || len(v) > maxSeedBytes {
		bad("seed")
	}
	if len(fields) > 0 {
		return plan{}, fields, "the request is invalid"
	}
	p := plan{
		disks:    []hostapi.DiskSpec{{Name: dataDiskName, SizeBytes: r.DataDisk}},
		userData: r.Seed,
		create: mvmd.Create{
			ID: r.Name, CPU: r.CPU, MemoryBytes: r.Memory, Policy: r.NetworkPolicy,
			BootSet: r.BootSet, Cmdline: r.Cmdline, RoutedPrefixes: routed, RestartOnBoot: restartOnBoot(r.RestartOnBoot, true), Fenced: r.Fenced,
			Disks: []mvmd.Attachment{{ID: diskID(r.Name, dataDiskName)}},
		},
	}
	return p, nil, ""
}

// networkConfig is the seed's network-config, version 1: one card, eth0, with the Machine's
// address and its gateway, the form init's netcfg accepts.
func networkConfig(address netip.Prefix, gateway netip.Addr) []byte {
	return fmt.Appendf(nil, `version: 1
config:
    - type: physical
      name: eth0
      subnets:
        - type: static
          address: '%s'
          netmask: '%s'
          gateway: '%s'
`, address.Addr(), net.IP(net.CIDRMask(address.Bits(), 32)), gateway)
}

// seedVolume is the NoCloud volume of a microVM: the user-data the request carried, a meta-data
// that names the instance, and the network-config.
func seedVolume(name string, userData []byte, address netip.Prefix, gateway netip.Addr) ([]byte, error) {
	return iso9660.Write(seedLabel, []iso9660.File{
		{Name: "user-data", Data: userData},
		{Name: "meta-data", Data: []byte("instance-id: " + name + "\nlocal-hostname: " + name + "\n")},
		{Name: "network-config", Data: networkConfig(address, gateway)},
	})
}
