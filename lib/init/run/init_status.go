package run

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/init/upgrade"
)

// writeInitStatus writes path, the body of hostapi.NaruInfo, which is the one definition of the
// file's shape, with the version, the slot and the last upgrade of u, atomically, so
// naru-hostd never reads half of it.
func writeInitStatus(path, version string, u *upgrade.Upgrader) error {
	st := hostapi.NaruInfo{Version: version, Slot: u.Slot()}
	if last, ok := u.Last(); ok {
		st.LastUpgrade = &hostapi.Upgrade{From: last.From, To: last.To, Result: last.Result, Reason: last.Reason, Time: last.Time}
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
