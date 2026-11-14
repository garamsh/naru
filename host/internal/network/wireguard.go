package network

import (
	"sort"
	"strconv"
	"strings"
)

// TunnelArgs returns the arguments of one `wg set` call that brings the tunnel to c's peers: the
// private key is read by wg from keyFile, so the key is never in the arguments, a peer in current
// (the public keys the interface has now) that c no longer lists is removed, and each listed peer
// has the endpoint and the allowed IPs of the prefixes it holds, replacing what it had.
func TunnelArgs(c Config, keyFile string, port int, current []string) []string {
	args := []string{"set", c.Tunnel, "listen-port", strconv.Itoa(port), "private-key", keyFile}
	peers := sortedPeers(c.Peers)
	wanted := map[string]bool{}
	for _, p := range peers {
		wanted[p.PublicKey] = true
	}
	gone := append([]string(nil), current...)
	sort.Strings(gone)
	for _, k := range gone {
		if !wanted[k] {
			args = append(args, "peer", k, "remove")
		}
	}
	for _, p := range peers {
		args = append(args, "peer", p.PublicKey)
		if p.Endpoint.IsValid() {
			args = append(args, "endpoint", p.Endpoint.String())
		}
		var ips []string
		for _, x := range sortedPrefixes(p.Prefixes) {
			ips = append(ips, x.String())
		}
		args = append(args, "allowed-ips", strings.Join(ips, ","))
	}
	return args
}
