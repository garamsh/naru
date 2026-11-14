// Command naru-keyprobe scans the Node's file systems for a private key that belongs to one of the
// public keys it is given, outside the directories it is told to skip, prints one line and stays
// running:
//
//	naru96-keyprobe: scanned=<files> found=<comma-separated paths, or none>
//
// The harness gives it the public halves of the authorities (docs/architecture/harness.md).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/garamsh/naru/tooling/qemu/internal/keyprobe"
)

func main() {
	pubFile := flag.String("public-pem", "", "file of PEM certificates and PKIX public keys whose private keys are looked for")
	skip := flag.String("skip", "/proc,/sys,/dev,/var/lib/naru/signer", "comma-separated directories not scanned")
	roots := flag.String("roots", "/", "comma-separated directories scanned")
	flag.Parse()
	raw, err := os.ReadFile(*pubFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	want, err := keyprobe.Parse(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	n, found := keyprobe.Scan(strings.Split(*roots, ","), strings.Split(*skip, ","), want)
	if len(found) == 0 {
		found = []string{"none"}
	}
	fmt.Printf("naru96-keyprobe: scanned=%d found=%s\n", n, strings.Join(found, ","))
	select {}
}
