package linux

import (
	"fmt"
	"os"
	"strings"
)

// BootIDPath is where the kernel exposes the ID it made for this boot.
const BootIDPath = "/proc/sys/kernel/random/boot_id"

// ReadBootID returns the boot ID the file at path holds.
func ReadBootID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the boot ID: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read the boot ID: %s is empty", path)
	}
	return id, nil
}
