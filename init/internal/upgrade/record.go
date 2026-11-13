package upgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// What an upgrade ended as.
const (
	ResultPromoted   = "promoted"
	ResultRolledBack = "rolled back"
)

// The files of the data disk's upgrade directory.
const (
	pendingFile = "pending.json"
	lastFile    = "last.json"
)

// Outcome is how the last upgrade ended: the versions it went from and to, what became of it, why
// when it was rolled back, and when init decided.
type Outcome struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	// Time is RFC 3339.
	Time string `json:"time"`
}

// pending is an upgrade that has been staged and has not ended: written before the trial entry is
// set, removed when the upgrade ends. Reason is set by a trial boot that gives up.
type pending struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

func (u *Upgrader) readPending() (pending, bool) {
	var p pending
	if err := readJSON(filepath.Join(u.cfg.StateDir, pendingFile), &p); err != nil {
		return pending{}, false
	}
	return p, true
}

func (u *Upgrader) writePending(p pending) error {
	return writeJSON(u.cfg.StateDir, pendingFile, p)
}

func (u *Upgrader) removePending() error {
	err := os.Remove(filepath.Join(u.cfg.StateDir, pendingFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// finish records how the upgrade ended and removes what staged it.
func (u *Upgrader) finish(o Outcome) error {
	o.Time = u.cfg.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := writeJSON(u.cfg.StateDir, lastFile, o); err != nil {
		return err
	}
	return u.removePending()
}

// Last returns the outcome of the last upgrade that ended, and false when none has.
func (u *Upgrader) Last() (Outcome, bool) {
	var o Outcome
	if err := readJSON(filepath.Join(u.cfg.StateDir, lastFile), &o); err != nil {
		return Outcome{}, false
	}
	return o, true
}

func readJSON(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// writeJSON writes dir/name atomically: a temporary file, fsync, rename and fsync of the
// directory, as the configuration store does.
func writeJSON(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".upgrade-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}
