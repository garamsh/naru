package main

import (
	"fmt"
	"io"
	"os"

	"github.com/garamsh/naru/init/internal/config"
)

// validate checks the naru.yaml at path the way init does at boot, writes the result to out and
// returns the exit status. It is how a tool outside this module asks whether a file is one init
// accepts.
func validate(path string, out, errOut io.Writer) int {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(errOut, "naru-init:", err)
		return 1
	}
	cfg, err := config.Parse(b)
	if err != nil {
		fmt.Fprintf(errOut, "naru-init: %s: invalid configuration: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(out, "naru-init: %s: valid; node=%s services=%d\n", path, cfg.Metadata.Name, len(cfg.Spec.Services))
	return 0
}
