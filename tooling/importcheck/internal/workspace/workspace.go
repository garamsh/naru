// Package workspace reads the go.work of a repository root: the one list of modules every check of
// the repository covers.
package workspace

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// Work is what a go.work declares.
type Work struct {
	// GoVersion is the go directive.
	GoVersion string
	// Use holds the directory of each used module, relative to the root and slash-separated.
	Use []string
}

// Read parses root/go.work. A go.work with no go directive or no use directive is an error.
func Read(root string) (Work, error) {
	file := filepath.Join(root, "go.work")
	b, err := os.ReadFile(file)
	if err != nil {
		return Work{}, err
	}
	f, err := modfile.ParseWork(file, b, nil)
	if err != nil {
		return Work{}, err
	}
	if f.Go == nil {
		return Work{}, fmt.Errorf("%s has no go directive", file)
	}
	w := Work{GoVersion: f.Go.Version}
	for _, u := range f.Use {
		w.Use = append(w.Use, path.Clean(filepath.ToSlash(u.Path)))
	}
	if len(w.Use) == 0 {
		return Work{}, fmt.Errorf("%s has no use directive", file)
	}
	return w, nil
}
