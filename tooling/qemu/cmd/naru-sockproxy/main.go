// Command naru-sockproxy forwards TCP connections to a unix socket, for the harness: the signer's
// socket is a file in the guest, and the test host reaches it through a TCP forward of the Node's
// second NIC (docs/architecture/harness.md). It is a test tool and no part of the image.
//
//	naru-sockproxy <listen address> <unix socket>
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: naru-sockproxy <listen address> <unix socket>")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("naru95-sockproxy: %s to %s\n", os.Args[1], os.Args[2])
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		go func() {
			defer func() { _ = c.Close() }()
			u, err := net.DialTimeout("unix", os.Args[2], 5*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = u.Close() }()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
			<-done
		}()
	}
}
