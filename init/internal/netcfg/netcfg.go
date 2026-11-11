// Package netcfg parses the network-config file of the NoCloud drive, in the
// version 1 format Proxmox writes (docs/architecture/config.md). It accepts
// what init can apply, static IPv4 addresses and resolvers, and refuses every
// other thing the format can say, so a Node never boots with a network it was
// not given.
package netcfg

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"go.yaml.in/yaml/v4"
)

// FileName is the file on the NoCloud drive.
const FileName = "network-config"

// Config is a parsed network-config.
type Config struct {
	Interfaces  []Interface
	Nameservers []netip.Addr
	Search      []string
}

// Interface is one network interface and the IPv4 configuration it is given.
// It is found by MAC when MAC is set, by Name otherwise.
type Interface struct {
	Name      string
	MAC       net.HardwareAddr
	Addresses []netip.Prefix
	// Gateway is the default gateway, or the zero Addr when there is none.
	Gateway netip.Addr
}

type document struct {
	Version int     `yaml:"version"`
	Config  []entry `yaml:"config"`
}

type entry struct {
	Type       string   `yaml:"type"`
	Name       string   `yaml:"name"`
	MACAddress string   `yaml:"mac_address"`
	Subnets    []subnet `yaml:"subnets"`
	Address    []string `yaml:"address"`
	Search     []string `yaml:"search"`
}

type subnet struct {
	Type    string `yaml:"type"`
	Address string `yaml:"address"`
	Netmask string `yaml:"netmask"`
	Gateway string `yaml:"gateway"`
}

// Parse returns the configuration data holds, or an error joining each thing
// in it that is unknown, unsupported or invalid.
func Parse(data []byte) (Config, error) {
	var doc document
	if err := yaml.Load(data, &doc, yaml.WithKnownFields()); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if doc.Version != 1 {
		return Config{}, fmt.Errorf("%s: version must be 1, got %d", FileName, doc.Version)
	}
	var cfg Config
	var errs []error
	fail := func(i int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: config[%d]: %s", FileName, i, fmt.Sprintf(format, args...)))
	}
	nameservers := 0
	for i, e := range doc.Config {
		switch e.Type {
		case "physical":
			iface, err := physical(e)
			if err != nil {
				fail(i, "%v", err)
				continue
			}
			cfg.Interfaces = append(cfg.Interfaces, iface)
		case "nameserver":
			nameservers++
			if nameservers > 1 {
				fail(i, "a second nameserver entry")
				continue
			}
			ns, err := nameserver(e)
			if err != nil {
				fail(i, "%v", err)
				continue
			}
			cfg.Nameservers, cfg.Search = ns, e.Search
		default:
			fail(i, "type %q is not supported", e.Type)
		}
	}
	if len(errs) == 0 && len(cfg.Interfaces) == 0 {
		errs = append(errs, fmt.Errorf("%s: configures no interface", FileName))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func physical(e entry) (Interface, error) {
	iface := Interface{Name: e.Name}
	if len(e.Address) > 0 || len(e.Search) > 0 {
		return iface, errors.New("a physical entry takes no address or search")
	}
	if e.Name == "" && e.MACAddress == "" {
		return iface, errors.New("names no interface: give name or mac_address")
	}
	if e.MACAddress != "" {
		mac, err := net.ParseMAC(e.MACAddress)
		if err != nil || len(mac) != 6 {
			return iface, fmt.Errorf("mac_address %q is not an Ethernet address", e.MACAddress)
		}
		iface.MAC = mac
	}
	if len(e.Subnets) == 0 {
		return iface, errors.New("has no subnets")
	}
	for i, s := range e.Subnets {
		if s.Type != "static" {
			return iface, fmt.Errorf("subnets[%d]: type %q is not supported, only static IPv4", i, s.Type)
		}
		prefix, err := staticPrefix(s)
		if err != nil {
			return iface, fmt.Errorf("subnets[%d]: %w", i, err)
		}
		iface.Addresses = append(iface.Addresses, prefix)
		if s.Gateway == "" {
			continue
		}
		gw, err := netip.ParseAddr(s.Gateway)
		if err != nil || !gw.Is4() {
			return iface, fmt.Errorf("subnets[%d]: gateway %q is not an IPv4 address", i, s.Gateway)
		}
		if !prefix.Contains(gw) {
			return iface, fmt.Errorf("subnets[%d]: gateway %s is outside %s", i, gw, prefix.Masked())
		}
		if iface.Gateway.IsValid() {
			return iface, errors.New("has more than one gateway")
		}
		iface.Gateway = gw
	}
	return iface, nil
}

// staticPrefix returns the address and prefix length of a static subnet, given
// as address/length or as an address and a dotted netmask, which is how
// Proxmox writes it.
func staticPrefix(s subnet) (netip.Prefix, error) {
	if strings.Contains(s.Address, "/") {
		if s.Netmask != "" {
			return netip.Prefix{}, errors.New("gives the prefix length twice, in address and in netmask")
		}
		p, err := netip.ParsePrefix(s.Address)
		if err != nil || !p.Addr().Is4() {
			return netip.Prefix{}, fmt.Errorf("address %q is not an IPv4 address with a prefix length", s.Address)
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s.Address)
	if err != nil || !a.Is4() {
		return netip.Prefix{}, fmt.Errorf("address %q is not an IPv4 address", s.Address)
	}
	m, err := netip.ParseAddr(s.Netmask)
	if err != nil || !m.Is4() {
		return netip.Prefix{}, fmt.Errorf("netmask %q is not a dotted IPv4 netmask", s.Netmask)
	}
	ones, bits := net.IPMask(m.AsSlice()).Size()
	if bits == 0 {
		return netip.Prefix{}, fmt.Errorf("netmask %q is not contiguous", s.Netmask)
	}
	return netip.PrefixFrom(a, ones), nil
}

func nameserver(e entry) ([]netip.Addr, error) {
	if e.Name != "" || e.MACAddress != "" || len(e.Subnets) > 0 {
		return nil, errors.New("a nameserver entry takes only address and search")
	}
	for _, d := range e.Search {
		// A domain with a space would make resolv.conf's search line name several.
		if d == "" || strings.ContainsAny(d, " \t\r\n#;\x00") {
			return nil, fmt.Errorf("search domain %q is not a domain name", d)
		}
	}
	var out []netip.Addr
	for _, s := range e.Address {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("nameserver %q is not an IP address", s)
		}
		out = append(out, a)
	}
	return out, nil
}
