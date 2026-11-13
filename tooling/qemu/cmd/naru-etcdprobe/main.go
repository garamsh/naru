// Command naru-etcdprobe is the service the QEMU test of the control plane's HA configures (ADR
// 0024). It runs on a Node and, every few seconds, prints what a client sees of the control plane
// from two source addresses: the Node's own, a member's, and an alias it adds to its card, which no
// member list names. It prints one line for each etcd member it dials and one for the virtual
// address, and the test reads them from the console. It speaks to etcd with nothing of etcd's own:
// the member list is the Cluster service's answer, read from the protobuf by hand.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	siocsifaddr    = 0x8916
	siocsifnetmask = 0x891c
)

func main() {
	var (
		members = flag.String("member", "", "comma-separated addresses of the control-plane Nodes")
		vip     = flag.String("vip", "", "the control plane's virtual address")
		alias   = flag.String("alias", "", "an address, with no member's, to add to eth0 and dial from")
		ca      = flag.String("ca", "/run/gaon/ca.pem", "the cluster's CA certificate")
		cert    = flag.String("cert", "/run/gaon/apiserver-etcd.pem", "the client certificate to present")
		key     = flag.String("key", "/run/gaon/apiserver-etcd.key", "its key")
		machine = flag.String("machine-alias", "", "an address, in this Node's MicroVM prefix, to add to eth0 and dial from")
		snapCrt = flag.String("snapshot-cert-pem", "", "the PEM of the pair naructl gen etcd-client wrote")
		snapKey = flag.String("snapshot-key-pem", "", "its key, PEM")
	)
	flag.Parse()
	if err := addAlias(*alias, "eth0:59"); err != nil {
		fmt.Println("naru59-probe: failed to add the alias:", err)
		os.Exit(1)
	}
	machineAddr := strings.Split(*machine, "/")[0]
	if *machine != "" {
		if err := addAlias(*machine, "eth0:60"); err != nil {
			fmt.Println("naru59-probe: failed to add the machine alias:", err)
			os.Exit(1)
		}
	}
	pool, pair, err := load(*ca, *cert, *key)
	if err != nil {
		fmt.Println("naru59-probe: failed to read the certificates:", err)
		os.Exit(1)
	}
	var snap tls.Certificate
	if *snapCrt != "" {
		if snap, err = tls.X509KeyPair([]byte(*snapCrt), []byte(*snapKey)); err != nil {
			fmt.Println("naru59-probe: failed to read the snapshot pair:", err)
			os.Exit(1)
		}
	}
	for {
		probeOnce(strings.Split(*members, ","), *vip, *alias, pool, pair)
		if *snapCrt != "" {
			probeSnapshot(strings.Split(*members, ","), machineAddr, pool, snap)
		}
		time.Sleep(5 * time.Second)
	}
}

func load(ca, cert, key string) (*x509.CertPool, tls.Certificate, error) {
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	pair, err := tls.LoadX509KeyPair(cert, key)
	return pool, pair, err
}

// addAlias adds addr, a CIDR, to eth0 as the alias label with the ioctls ifconfig uses.
func addAlias(cidr, label string) error {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	set := func(req uintptr, v net.IP) error {
		var ifr [40]byte
		copy(ifr[:], label)
		binary.LittleEndian.PutUint16(ifr[16:], syscall.AF_INET)
		copy(ifr[20:24], v.To4())
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
			return e
		}
		return nil
	}
	if err := set(siocsifaddr, ip); err != nil {
		return err
	}
	return set(siocsifnetmask, net.IP(n.Mask))
}

// probeSnapshot dials as gaon's snapshot writer does, with the pair naructl gen etcd-client wrote.
// From this Node's own address and from an address in this Node's MicroVM prefix, every member
// serves it on 2379. From the MicroVM prefix the peer port is refused by a reset at every member:
// this Node's naru-fence rejects it before the masquerade.
func probeSnapshot(members []string, machine string, pool *x509.CertPool, snap tls.Certificate) {
	for _, m := range members {
		status, err := get("", m+":2379", "/version", pool, []tls.Certificate{snap})
		fmt.Printf("naru59-probe: etcd=%s source=member client-cert=snapshot result=%s\n", m, classify(status, err))
		if machine == "" {
			continue
		}
		status, err = get(machine, m+":2379", "/version", pool, []tls.Certificate{snap})
		fmt.Printf("naru59-probe: etcd=%s source=machine client-cert=snapshot result=%s\n", m, classify(status, err))
		status, err = get(machine, m+":2380", "/version", pool, []tls.Certificate{snap})
		fmt.Printf("naru59-probe: etcd-peer=%s source=machine client-cert=snapshot result=%s\n", m, classify(status, err))
	}
}

func probeOnce(members []string, vip, alias string, pool *x509.CertPool, pair tls.Certificate) {
	own := map[string]tls.Certificate{"yes": pair, "no": {}}
	for _, m := range members {
		for _, source := range []struct{ name, addr string }{{"member", ""}, {"alias", strings.Split(alias, "/")[0]}} {
			for _, withCert := range []string{"yes", "no"} {
				if source.name == "alias" && withCert == "no" {
					continue
				}
				status, err := get(source.addr, m+":2379", "/version", pool, certs(withCert, own))
				fmt.Printf("naru59-probe: etcd=%s source=%s client-cert=%s result=%s\n", m, source.name, withCert, classify(status, err))
			}
		}
	}
	for _, m := range members {
		names, err := memberList(m+":2379", pool, pair)
		if err == nil {
			fmt.Printf("naru59-probe: members from=%s names=%s\n", m, strings.Join(names, ","))
		}
	}
	status, err := get("", vip+":6443", "/apis/compute.gaon.garam.sh/v1alpha1/nodes", pool, []tls.Certificate{pair})
	fmt.Printf("naru59-probe: vip=%s apiserver=%s\n", vip, classify(status, err))
}

func certs(with string, own map[string]tls.Certificate) []tls.Certificate {
	if with == "no" {
		return nil
	}
	return []tls.Certificate{own[with]}
}

// classify names how a request ended. Any answer from the server is "answered"; only a status 200
// is "connected".
func classify(status int, err error) string {
	switch {
	case err == nil && status == http.StatusOK:
		return "connected"
	case err == nil:
		return fmt.Sprintf("answered-%d", status)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused-by-tcp-reset"
	case strings.Contains(err.Error(), "certificate required") || strings.Contains(err.Error(), "bad certificate"):
		return "refused-at-tls-handshake"
	case strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline"):
		return "timeout"
	}
	return "error: " + err.Error()
}

func client(source string, pool *x509.CertPool, cs []tls.Certificate) *http.Client {
	d := &net.Dialer{Timeout: 3 * time.Second}
	if source != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext:       d.DialContext,
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, Certificates: cs, MinVersion: tls.VersionTLS12},
		},
	}
}

func get(source, hostport, path string, pool *x509.CertPool, cs []tls.Certificate) (int, error) {
	c := client(source, pool, cs)
	defer c.CloseIdleConnections()
	resp, err := c.Get("https://" + hostport + path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// memberList asks etcd's Cluster service for its members: a gRPC call whose request is empty, and
// whose answer is a ResponseHeader (field 1) and the Members (field 2), each with an ID (1), a name
// (2), peer URLs (3) and client URLs (4).
func memberList(hostport string, pool *x509.CertPool, pair tls.Certificate) ([]string, error) {
	c := client("", pool, []tls.Certificate{pair})
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+hostport+"/etcdserverpb.Cluster/MemberList", strings.NewReader("\x00\x00\x00\x00\x00"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if st := resp.Trailer.Get("Grpc-Status"); st != "" && st != "0" {
		return nil, fmt.Errorf("grpc status %s", st)
	}
	if len(body) < 5 {
		return nil, errors.New("short answer")
	}
	var names []string
	for _, m := range fields(body[5:], 2) {
		for _, n := range fields(m, 2) {
			names = append(names, string(n))
		}
	}
	return names, nil
}

// fields returns the length-delimited fields numbered num in the protobuf message b.
func fields(b []byte, num uint64) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(b)
		case 1:
			n = 8
		case 5:
			n = 4
		case 2:
			l, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < l {
				return out
			}
			if tag>>3 == num {
				out = append(out, b[k:k+int(l)])
			}
			n = k + int(l)
		default:
			return out
		}
		if n <= 0 || n > len(b) {
			return out
		}
		b = b[n:]
	}
	return out
}
