package run

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/init/upgrade"
)

// initStatus is what init tells naru-hostd of itself in config.InitStatusFile.
type initStatus struct {
	Version     string           `json:"version"`
	Slot        string           `json:"slot"`
	LastUpgrade *upgrade.Outcome `json:"lastUpgrade,omitempty"`
}

// writeInitStatus writes path with the version, the slot and the last upgrade of u, atomically, so
// naru-hostd never reads half of it.
func writeInitStatus(path, version string, u *upgrade.Upgrader) error {
	st := initStatus{Version: version, Slot: u.Slot()}
	if last, ok := u.Last(); ok {
		st.LastUpgrade = &last
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o644)
}
