// Command naru-entry is the entrypoint of the test image a Machine or a Pod runs. With no arguments
// it prints one line, serves probe on defaultProbe and stays running: hostd refuses a command for a
// microVM, so a microVM of the network tests (docs/architecture/harness.md) runs probe by default.
//
// The Kubernetes test (ADR 0031) gives it arguments, so that one image is the server and the client:
//
//	naru-entry serve <address>       answer every HTTP request with a line, and print each peer
//	naru-entry probe <address>       serve, and act from inside the microVM for the network tests (probe.go)
//	naru-entry dial <url>...         fetch each URL until it answers, print one line per URL, and stay running
//	naru-entry resolve <name>...     look each name up through the resolver in /etc/resolv.conf until it answers,
//	                                 print one line per name, and stay running (the DNS test, ADR 0083)
//	naru-entry dns <address> <name>=<ip>...
//	                                 answer an A query for each name with its address on UDP <address>, as the
//	                                 resolver the Nodes' network-config names for the DNS test
//	naru-entry idle                  print one line and stay running: the command of the sandbox image
//	                                 (fixture.WriteRegistry), which a Pod's sandbox container runs
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// answer is what serve replies, and what dial looks for.
const answer = "naru91-entry-answer"

// defaultProbe is where probe listens when the image runs with no arguments.
const defaultProbe = "0.0.0.0:8080"

func main() {
	fmt.Println("hello from the entrypoint")
	switch {
	case len(os.Args) == 1:
		probe(defaultProbe)
	case len(os.Args) == 3 && os.Args[1] == "serve":
		serve(os.Args[2])
	case len(os.Args) == 3 && os.Args[1] == "probe":
		probe(os.Args[2])
	case len(os.Args) > 2 && os.Args[1] == "dial":
		for _, u := range os.Args[2:] {
			dial(u)
		}
	case len(os.Args) > 2 && os.Args[1] == "resolve":
		for _, name := range os.Args[2:] {
			resolve(name)
		}
	case len(os.Args) > 3 && os.Args[1] == "dns":
		answerDNS(os.Args[2], os.Args[3:])
	}
	time.Sleep(time.Hour)
}

// resolve looks name up every two seconds for ten minutes, and prints "naru91-entry: resolved <name> <addresses>"
// on the first answer.
func resolve(name string) {
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(ctx, name)
		cancel()
		if err != nil {
			fmt.Printf("naru91-entry: resolve %s: %v\n", name, err)
			continue
		}
		fmt.Printf("naru91-entry: resolved %s %s\n", name, strings.Join(addrs, ","))
		return
	}
	fmt.Printf("naru91-entry: gave up on %s\n", name)
}

// answerDNS serves records, each "<name>=<ip>", over UDP on addr in the background: an A query for a listed
// name gets its address, every other query an empty answer with the name-error code.
func answerDNS(addr string, records []string) {
	byName := map[string]netip.Addr{}
	for _, r := range records {
		name, ip, _ := strings.Cut(r, "=")
		a, err := netip.ParseAddr(ip)
		if err != nil {
			fmt.Printf("naru91-entry: dns record %q: %v\n", r, err)
			os.Exit(1)
		}
		byName[strings.ToLower(strings.TrimSuffix(name, "."))+"."] = a
	}
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		fmt.Println("naru91-entry: dns ended:", err)
		os.Exit(1)
	}
	fmt.Printf("naru91-entry: dns serving %s\n", addr)
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				fmt.Println("naru91-entry: dns ended:", err)
				os.Exit(1)
			}
			if reply, ok := dnsReply(buf[:n], byName); ok {
				_, _ = conn.WriteTo(reply, peer)
			}
		}
	}()
}

func dnsReply(query []byte, byName map[string]netip.Addr) ([]byte, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		return nil, false
	}
	h.Response, h.RecursionAvailable = true, true
	a, found := byName[strings.ToLower(q.Name.String())]
	hasA := q.Type == dnsmessage.TypeA && found && a.Is4()
	switch {
	case !found:
		h.RCode = dnsmessage.RCodeNameError
	default:
		h.RCode = dnsmessage.RCodeSuccess
	}
	b := dnsmessage.NewBuilder(nil, h)
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil, false
	}
	if hasA {
		if b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.AResource{A: a.As4()}) != nil {
			return nil, false
		}
	}
	reply, err := b.Finish()
	return reply, err == nil
}

func serve(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("naru91-entry: served %s\n", r.RemoteAddr)
		_, _ = io.WriteString(w, answer+"\n")
	})
	go func() {
		fmt.Printf("naru91-entry: serving %s\n", addr)
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
		fmt.Println("naru91-entry: serve ended:", srv.ListenAndServe())
		os.Exit(1)
	}()
}

// dial fetches u every two seconds for ten minutes, and prints "naru91-entry: dialed <u>" on the first answer.
func dial(u string) {
	client := &http.Client{Timeout: 3 * time.Second}
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		resp, err := client.Get(u)
		if err != nil {
			fmt.Printf("naru91-entry: dial %s: %v\n", u, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && string(body) == answer+"\n" {
			fmt.Printf("naru91-entry: dialed %s\n", u)
			return
		}
	}
	fmt.Printf("naru91-entry: gave up on %s\n", u)
}
