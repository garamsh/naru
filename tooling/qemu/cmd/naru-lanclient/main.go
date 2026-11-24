// Command naru-lanclient is the client outside the networks of the exposure test (#275): every two
// seconds it connects to each address:port it is given, reads the line the backend answers, and
// prints "naru94-lan: <address:port>=<answer>" for each, with "fail" when it got none
// (docs/architecture/image.md).
package main

import (
	"bufio"
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
	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		return "fail"
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "fail"
	}
	return strings.TrimSpace(line)
}
