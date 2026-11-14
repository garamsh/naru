package service

import (
	"crypto/sha256"
	"os"
	"time"
)

// watchInterval is how often the manager compares a running service's RestartOnChange files
// with what they held when the service started.
const watchInterval = 5 * time.Second

// digests returns what each of paths holds: the SHA-256 of its content, and the empty string for
// a file that is missing or unreadable, so a file that appears or goes missing differs from
// either state.
func digests(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			sum := sha256.Sum256(b)
			out[i] = string(sum[:])
		}
	}
	return out
}
