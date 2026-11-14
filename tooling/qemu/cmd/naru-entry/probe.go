package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// udpPort is where probe records the datagrams it receives, and where /send and /spoof send them.
const udpPort = 9999

// probe is the microVM of the network tests (docs/architecture/harness.md): the test host reaches
// its HTTP API through the harness route and has it act from inside the microVM.
//
//	/probe?target=ip:port    dial the TCP address from the microVM: "ok" or "fail: <why>"
//	/send?dst=ip             send one UDP datagram from the microVM's own address: "sent"
//	/spoof?src=ip&dst=ip     send one UDP datagram whose source is src, in a raw packet: "sent"
//	/received                the source addresses of the datagrams received, "<address> <count>" per line
//
// Every other path answers like serve, so the microVM is also a TCP target for the others.
func probe(addr string) {
	var mu sync.Mutex
	seen := map[string]int{}
	go receive(&mu, seen)

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		c, err := net.DialTimeout("tcp", r.URL.Query().Get("target"), 3*time.Second)
		if err != nil {
			fmt.Fprintf(w, "fail: %v\n", err)
			return
		}
		_ = c.Close()
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		c, err := net.Dial("udp", net.JoinHostPort(r.URL.Query().Get("dst"), fmt.Sprint(udpPort)))
		if err == nil {
			_, err = c.Write([]byte("naru94-datagram"))
			_ = c.Close()
		}
		reply(w, err)
	})
	mux.HandleFunc("/spoof", func(w http.ResponseWriter, r *http.Request) {
		reply(w, spoof(r.URL.Query().Get("src"), r.URL.Query().Get("dst")))
	})
	mux.HandleFunc("/received", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var lines []string
		for s, n := range seen {
			lines = append(lines, fmt.Sprintf("%s %d", s, n))
		}
		sort.Strings(lines)
		_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, answer+"\n") })
	go func() {
		fmt.Printf("naru94-probe: serving %s\n", addr)
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		fmt.Println("naru94-probe: serve ended:", srv.ListenAndServe())
	}()
}

func reply(w io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(w, "fail: %v\n", err)
		return
	}
	fmt.Fprintln(w, "sent")
}

func receive(mu *sync.Mutex, seen map[string]int) {
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", udpPort))
	if err != nil {
		fmt.Println("naru94-probe: listen udp:", err)
		return
	}
	buf := make([]byte, 2048)
	for {
		_, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		host, _, _ := net.SplitHostPort(from.String())
		mu.Lock()
		seen[host]++
		mu.Unlock()
	}
}

// spoof sends a UDP datagram whose source address is src in a raw IPv4 packet: the host must drop it
// when src is not the microVM's own.
func spoof(src, dst string) error {
	s, err := netip.ParseAddr(src)
	if err != nil {
		return err
	}
	d, err := netip.ParseAddr(dst)
	if err != nil {
		return err
	}
	payload := []byte("naru94-forged")
	pkt := make([]byte, 28+len(payload))
	pkt[0], pkt[8], pkt[9] = 0x45, 64, syscall.IPPROTO_UDP
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt)))
	s4, d4 := s.As4(), d.As4()
	copy(pkt[12:], s4[:])
	copy(pkt[16:], d4[:])
	binary.BigEndian.PutUint16(pkt[10:], ipChecksum(pkt[:20]))
	binary.BigEndian.PutUint16(pkt[20:], 40000)
	binary.BigEndian.PutUint16(pkt[22:], udpPort)
	binary.BigEndian.PutUint16(pkt[24:], uint16(8+len(payload)))
	copy(pkt[28:], payload) // the UDP checksum, left 0, is "none"
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	return syscall.Sendto(fd, pkt, 0, &syscall.SockaddrInet4{Addr: d4})
}

func ipChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
