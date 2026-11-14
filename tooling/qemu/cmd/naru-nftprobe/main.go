// Command naru-nftprobe prints, every two seconds, each counter of the Node's nftables ruleset that
// has a comment, as "naru94-nft: <comment>=<packets> ..." with the comment's spaces made underscores:
// the network tests read how many packets each drop rule counted (docs/architecture/harness.md).
package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var counter = regexp.MustCompile(`counter packets (\d+) bytes \d+ .*comment "([^"]+)"`)

func main() {
	for ; ; time.Sleep(2 * time.Second) {
		out, err := exec.Command("nft", "list", "ruleset").CombinedOutput()
		if err != nil {
			fmt.Printf("naru94-nft-error: %v: %s\n", err, strings.TrimSpace(string(out)))
			continue
		}
		var parts []string
		for _, m := range counter.FindAllStringSubmatch(string(out), -1) {
			parts = append(parts, strings.ReplaceAll(m[2], " ", "_")+"="+m[1])
		}
		fmt.Printf("naru94-nft: %s\n", strings.Join(parts, " "))
	}
}
