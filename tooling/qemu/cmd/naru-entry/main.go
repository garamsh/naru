// Command naru-entry is the entrypoint of the test image a Machine runs. With no arguments it prints
// one line, serves probe on defaultProbe and stays running: hostd refuses a command for a microVM,
// so a microVM of the network tests (docs/architecture/image.md) runs probe by default.
//
//	naru-entry probe <address>       serve, and act from inside the microVM for the network tests (probe.go)
package main

import (
	"fmt"
	"os"
	"time"
)

// answer is what probe's HTTP server replies.
const answer = "naru91-entry-answer"

// defaultProbe is where probe listens when the image runs with no arguments.
const defaultProbe = "0.0.0.0:8080"

func main() {
	fmt.Println("hello from the entrypoint")
	switch {
	case len(os.Args) == 1:
		probe(defaultProbe)
	case len(os.Args) == 3 && os.Args[1] == "probe":
		probe(os.Args[2])
	}
	time.Sleep(time.Hour)
}
