package volumes

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/lib/atomicfile"
)

const (
	localPrefix = "local-"
	blockPrefix = "block-"
	// firstProject is the lowest quota project the driver gives out, above the low numbers a
	// customer may use for their own.
	firstProject = 1 << 20
)

var errNoRecord = errors.New("no such volume")

func (d *Driver) recordPath(id string) string { return filepath.Join(d.cfg.StateDir, id+".json") }

// dataDir is the directory of a local volume.
func (d *Driver) dataDir(id string) string { return filepath.Join(d.cfg.StorageDir, Name, id) }

func (d *Driver) load(id string) (record, error) {
	var r record
	b, err := os.ReadFile(d.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return r, errNoRecord
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func (d *Driver) save(id string, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.cfg.StateDir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(d.recordPath(id), b, 0o600)
}

func (d *Driver) forget(id string) error {
	err := os.Remove(d.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// records returns every record, by volume id.
func (d *Driver) records() (map[string]record, error) {
	entries, err := os.ReadDir(d.cfg.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]record{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := d.load(id)
		if err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, nil
}

// nextProject is the project after the highest one a record holds.
func nextProject(all map[string]record) uint32 {
	next := uint32(firstProject)
	for _, r := range all {
		if r.Project >= next {
			next = r.Project + 1
		}
	}
	return next
}
