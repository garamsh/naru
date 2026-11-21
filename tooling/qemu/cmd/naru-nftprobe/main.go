// Command naru-nftprobe prints, every two seconds, each counter of the Node's nftables ruleset that
// has a comment, as "naru94-nft: <comment>=<packets> ..." with the comment's spaces made underscores,
// and each ingress chain of mvmd's netdev table as "naru94-nft-ingress: <chain>=<policy>:<source>,..."
// where each source is an IPv4 address the chain accepts IPv4 packets from: the network tests read how
// many packets each drop rule counted, and which tap's ingress drops all but its own address
// (docs/architecture/harness.md).
package main

import (
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	counter = regexp.MustCompile(`counter packets (\d+) bytes \d+ .*comment "([^"]+)"`)
	chain   = regexp.MustCompile(`(?s)chain (\S+) \{\s*type filter hook ingress device "[^"]+" priority \S+; policy (\w+);(.*?)\n\t\}`)
	dnat    = regexp.MustCompile(`ip daddr (\S+) .*?(tcp|udp) dport (\d+) dnat ip to numgen random mod \d+ map \{ ([^}]*) \}`)
	source  = regexp.MustCompile(`@ll,96,16 0x800 @nh,96,32 0x([0-9a-f]+) accept`)
)

func main() {
	for ; ; time.Sleep(2 * time.Second) {
		out, err := exec.Command("nft", "list", "ruleset").CombinedOutput()
		if err != nil {
			fmt.Printf("naru94-nft-error: %v: %s\n", err, strings.TrimSpace(string(out)))
			continue
		}
		var parts []string
		for _, m := range counter.FindAllStringSubmatch(string(out), -1) {
			parts = append(parts, strings.ReplaceAll(m[2], " ", "_")+"="+m[1])
		}
		fmt.Printf("naru94-nft: %s\n", strings.Join(parts, " "))

		var ingress []string
		for _, m := range chain.FindAllStringSubmatch(string(out), -1) {
			var srcs []string
			for _, s := range source.FindAllStringSubmatch(m[3], -1) {
				n, _ := strconv.ParseUint(s[1], 16, 32)
				srcs = append(srcs, netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}).String())
			}
			ingress = append(ingress, m[1]+"="+m[2]+":"+strings.Join(srcs, ","))
		}
		fmt.Printf("naru94-nft-ingress: %s\n", strings.Join(ingress, " "))

		var dnats []string
		for _, m := range dnat.FindAllStringSubmatch(string(out), -1) {
			dnats = append(dnats, m[1]+":"+m[3]+"/"+m[2]+"="+strings.ReplaceAll(m[4], " ", ""))
		}
		fmt.Printf("naru94-nft-dnat: %s\n", strings.Join(dnats, " "))
		fmt.Printf("naru94-lan-addr: %s\n", addresses())
	}
}

// addresses lists the IPv4 addresses of each interface.
func addresses() string {
	ifs, _ := net.Interfaces()
	var parts []string
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		var ips []string
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
				ips = append(ips, p.String())
			}
		}
		parts = append(parts, i.Name+"="+strings.Join(ips, ","))
	}
	return strings.Join(parts, " ")
}
