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
	// firstProject is the lowest quota project the driver gives out, above the low numbers a
	// customer may use for their own.
	firstProject = 1 << 20
)

var errNoRecord = errors.New("no such volume")

func (d *Driver) recordPath(id volumeID) string {
	return filepath.Join(d.cfg.StateDir, string(id)+".json")
}

// dataDir is the directory of a local volume.
func (d *Driver) dataDir(id volumeID) string {
	return filepath.Join(d.cfg.StorageDir, Name, string(id))
}

func (d *Driver) load(id volumeID) (record, error) {
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

func (d *Driver) save(id volumeID, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.cfg.StateDir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(d.recordPath(id), b, 0o600)
}

func (d *Driver) forget(id volumeID) error {
	err := os.Remove(d.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// records returns every record, by volume id, and the ids of the records it cannot read. A file that
// is no volume's record is ignored.
func (d *Driver) records() (all map[volumeID]record, corrupt []volumeID, err error) {
	entries, err := os.ReadDir(d.cfg.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	all = map[volumeID]record{}
	for _, e := range entries {
		file, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		id, err := parseID(file)
		if err != nil {
			continue
		}
		r, err := d.load(id)
		if err != nil {
			corrupt = append(corrupt, id)
			continue
		}
		all[id] = r
	}
	return all, corrupt, nil
}

// nextProject is the project after the highest one a record holds.
func nextProject(all map[volumeID]record) uint32 {
	next := uint32(firstProject)
	for _, r := range all {
		if r.Project >= next {
			next = r.Project + 1
		}
	}
	return next
}
