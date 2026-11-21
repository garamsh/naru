package volumes

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config says what the Node offers. The Node's layout writes it.
type Config struct {
	// StateDir keeps one record per volume, outside the storage disk's quota.
	StateDir string `json:"stateDir"`
	// StorageDir is where the first storage disk is mounted, or empty when the Node has none.
	StorageDir string `json:"storageDir,omitempty"`
	// BlockSerials are the serial numbers of the disks the Node offers whole, in the order they are
	// handed out.
	BlockSerials []string `json:"blockSerials,omitempty"`
}

// LoadConfig reads the configuration file at path.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.StateDir == "" {
		return Config{}, fmt.Errorf("%s: stateDir is empty", path)
	}
	return c, nil
}
