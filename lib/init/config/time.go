package config

import (
	"net"
	"net/netip"
	"regexp"
	"strconv"
)

// DefaultTimePort is the port of an SNTP server that spec.time.servers names without one.
const DefaultTimePort = "123"

var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// Time is how the Node keeps its clock: init sets it from these servers over SNTP
// (docs/architecture/adr/0021-init-keeps-time-from-spec-time-servers-over-sntp.md).
type Time struct {
	// Servers are SNTP servers in the order they are asked: a host name or an IP address, with
	// a port after a colon when it is not DefaultTimePort. An IPv6 address with a port is
	// bracketed.
	Servers []string `yaml:"servers"`
}

// Addrs returns each server as host:port, in order, none for a nil Time. It is only meaningful
// once Parse has accepted the configuration.
func (t *Time) Addrs() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.Servers))
	for _, s := range t.Servers {
		addr, _ := serverAddr(s)
		out = append(out, addr)
	}
	return out
}

// serverAddr returns s as host:port, or the reason it is not a server.
func serverAddr(s string) (string, error) {
	host, port := s, DefaultTimePort
	if _, err := netip.ParseAddr(s); err != nil {
		if h, p, err := net.SplitHostPort(s); err == nil {
			n, err := strconv.ParseUint(p, 10, 16)
			if err != nil || n == 0 {
				return "", &portError{p}
			}
			host, port = h, p
		}
		if _, err := netip.ParseAddr(host); err != nil && (len(host) > 253 || !hostRE.MatchString(host)) {
			return "", &hostError{s}
		}
	}
	return net.JoinHostPort(host, port), nil
}

type portError struct{ port string }

func (e *portError) Error() string {
	return "the port " + strconv.Quote(e.port) + " is not between 1 and 65535"
}

type hostError struct{ server string }

func (e *hostError) Error() string {
	return strconv.Quote(e.server) + " is not a host name or an IP address, optionally with a port"
}

func (v *validation) time(t *Time) {
	if t == nil {
		return
	}
	if len(t.Servers) == 0 {
		v.fail("spec.time.servers", "must name at least one server")
		return
	}
	seen := map[string]bool{}
	for i, s := range t.Servers {
		field := "spec.time.servers[" + strconv.Itoa(i) + "]"
		addr, err := serverAddr(s)
		switch {
		case err != nil:
			v.fail(field, "%v", err)
		case seen[addr]:
			v.fail(field, "names %s again", addr)
		default:
			seen[addr] = true
		}
	}
}
