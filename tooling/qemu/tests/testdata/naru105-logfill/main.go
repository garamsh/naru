// Command naru105-logfill is a test-only service that prints enough lines, at a pace a service can
// keep, to fill a Node's kept console log (docs/architecture/adr/0036), then waits. It prints
// "naru105-logfill: done" last, which a test reads back to know the log was filled.
package main

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

func main() {
	var (
		lines = flag.Int("lines", 8000, "lines to print")
		size  = flag.Int("size", 160, "bytes of each line, newline included")
		rate  = flag.Int("rate", 400, "lines per second")
	)
	flag.Parse()
	pad := strings.Repeat("x", max(*size-32, 0))
	tick := time.NewTicker(time.Second / time.Duration(*rate))
	for i := 1; i <= *lines; i++ {
		<-tick.C
		fmt.Printf("naru105-logfill: %06d %s\n", i, pad)
	}
	fmt.Println("naru105-logfill: done")
	select {}
}
