// Command naru-lanclient is the client outside the networks of the exposure test (#275): every two
// seconds it connects to each address:port it is given, reads the line the backend answers, and
// prints "naru94-lan: <address:port>=<answer>" for each, with "fail" when it got none
// (docs/architecture/image.md). A target given as http://<address:port> is sent a plain HTTP GET
// first, and its answer is the response's status code. One given as tls://<address:port> is
// offered a TLS handshake for naru94.m.naru94.example, and its answer is "tls" when the server
// completed it, the name of the alert it refused it with, spaces made underscores, or "fail".
package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	targets := os.Args[1:]
	for ; ; time.Sleep(2 * time.Second) {
		var parts []string
		for _, t := range targets {
			parts = append(parts, t+"="+ask(t))
		}
		fmt.Printf("naru94-lan: %s\n", strings.Join(parts, " "))
	}
}

func ask(target string) string {
	if addr, ok := strings.CutPrefix(target, "tls://"); ok {
		return handshake(addr)
	}
	addr, plainHTTP := strings.CutPrefix(target, "http://")
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "fail"
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if plainHTTP {
		if _, err := fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: naru94.example\r\nConnection: close\r\n\r\n"); err != nil {
			return "fail"
		}
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "fail"
	}
	if plainHTTP {
		// "HTTP/1.1 308 Permanent Redirect": the status code.
		if f := strings.Fields(line); len(f) >= 2 {
			return f[1]
		}
		return "fail"
	}
	return strings.TrimSpace(line)
}

// handshake offers TLS to addr without checking the server's certificate: what is observed is who
// answers, not whom to trust.
func handshake(addr string) string {
	d := &net.Dialer{Timeout: 2 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: "naru94.m.naru94.example", InsecureSkipVerify: true})
	if err == nil {
		_ = c.Close()
		return "tls"
	}
	// An alert the server sent is a "remote error" whose text is "tls: <alert's name>".
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return strings.ReplaceAll(strings.TrimPrefix(op.Err.Error(), "tls: "), " ", "_")
	}
	return "fail"
}
