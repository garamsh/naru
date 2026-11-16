// Package importcheck enforces the import rule of docs/architecture/structure.md: a unit is a
// top-level folder, a unit imports no other unit, and lib/ imports no unit.
package importcheck

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/garamsh/naru/tooling/importcheck/internal/workspace"
)

// libUnit is the top-level folder whose modules every unit may import.
const libUnit = "lib"

// Violation is one import a file may not make.
type Violation struct {
	// File is the importing file, relative to the root.
	File string
	// Import is the import path.
	Import string
	// FromUnit and ToUnit are the top-level folders of the importing file's module and of the
	// module the path belongs to.
	FromUnit, ToUnit string
}

func (v Violation) String() string {
	if v.FromUnit == libUnit {
		return fmt.Sprintf("%s: lib/ imports %s, a package of unit %s/; lib/ imports no unit", v.File, v.Import, v.ToUnit)
	}
	return fmt.Sprintf("%s: unit %s/ imports %s, a package of unit %s/; a unit imports only lib/ and external modules", v.File, v.FromUnit, v.Import, v.ToUnit)
}

// Result is what Check examined and what it found.
type Result struct {
	// Modules and Files count the modules found under the root and the Go files read in them,
	// build-tagged and test files included.
	Modules, Files int
	Violations     []Violation
	// Unlisted holds the folders of modules found by go.mod that go.work does not use, and Missing
	// the folders go.work uses that hold no go.mod. Every other check covers the modules go.work
	// lists, so a module in either set is covered by this check and not by the rest, or by the
	// rest and not by this one.
	Unlisted, Missing []string
}

type module struct {
	dir  string // relative to the root, slash-separated
	path string // the module path
	unit string // the first element of dir
}

// Check reads every Go file of every module under root and reports the imports of a package that
// belongs to a module of another unit, except where the imported module is in lib/. It reads
// imports with go/parser, not `go list`, so a file excluded by a build tag or a test file is read as
// any other, and compares the modules it finds with the use list of the root's go.work. A root with
// no module or no go.work is an error.
func Check(root string) (Result, error) {
	mods, err := findModules(root)
	if err != nil {
		return Result{}, err
	}
	if len(mods) == 0 {
		return Result{}, fmt.Errorf("no go.mod under %s", root)
	}
	res := Result{Modules: len(mods)}
	work, err := workspace.Read(root)
	if err != nil {
		return Result{}, err
	}
	res.Unlisted, res.Missing = compare(mods, work.Use)
	err = walk(root, func(rel string, d fs.DirEntry) error {
		if d.IsDir() || !strings.HasSuffix(rel, ".go") {
			return nil
		}
		from := owner(mods, rel)
		if from == nil {
			return nil
		}
		res.Files++
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("read the imports of %s: %w", rel, err)
		}
		for _, spec := range file.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: import %s: %w", rel, spec.Path.Value, err)
			}
			to := importedModule(mods, p)
			if to == nil || to.unit == from.unit || to.unit == libUnit {
				continue
			}
			res.Violations = append(res.Violations, Violation{File: rel, Import: p, FromUnit: from.unit, ToUnit: to.unit})
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.Slice(res.Violations, func(i, j int) bool {
		a, b := res.Violations[i], res.Violations[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Import < b.Import
	})
	return res, nil
}

// compare returns the folders of mods that use does not list, and the folders of use that no module
// of mods has.
func compare(mods []*module, use []string) (unlisted, missing []string) {
	found := map[string]bool{}
	for _, m := range mods {
		found[m.dir] = true
	}
	listed := map[string]bool{}
	for _, u := range use {
		listed[u] = true
		if !found[u] {
			missing = append(missing, u)
		}
	}
	for _, m := range mods {
		if !listed[m.dir] {
			unlisted = append(unlisted, m.dir)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(missing)
	return unlisted, missing
}

func findModules(root string) ([]*module, error) {
	var mods []*module
	err := walk(root, func(rel string, d fs.DirEntry) error {
		if d.IsDir() || d.Name() != "go.mod" {
			return nil
		}
		path, err := modulePath(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		dir := strings.TrimSuffix(strings.TrimSuffix(rel, "go.mod"), "/")
		if dir == "" {
			return errors.New("go.mod at the root: the root holds no Go package (docs/architecture/structure.md)")
		}
		unit, _, _ := strings.Cut(dir, "/")
		mods = append(mods, &module{dir: dir, path: path, unit: unit})
		return nil
	})
	return mods, err
}

func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	p := modfile.ModulePath(b)
	if p == "" {
		return "", fmt.Errorf("%s has no module line", gomod)
	}
	return p, nil
}

// owner returns the module whose folder is the nearest one above the file.
func owner(mods []*module, file string) *module {
	var best *module
	for _, m := range mods {
		if strings.HasPrefix(file, m.dir+"/") && (best == nil || len(m.dir) > len(best.dir)) {
			best = m
		}
	}
	return best
}

// importedModule returns the module of the workspace the import path is in, the longest module path
// that is the path or a prefix of it at an element boundary.
func importedModule(mods []*module, importPath string) *module {
	var best *module
	for _, m := range mods {
		if (importPath == m.path || strings.HasPrefix(importPath, m.path+"/")) && (best == nil || len(m.path) > len(best.path)) {
			best = m
		}
	}
	return best
}

// walk calls visit for every file and folder under root, with its slash-separated path relative to
// root, and does not enter a folder that is not source: one whose name begins with "." or "_", a
// testdata folder, or the root's build/ and bin/, where the repository keeps its outputs.
func walk(root string, visit func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || (rel == name && (name == "build" || name == "bin")) {
				return filepath.SkipDir
			}
		}
		return visit(rel, d)
	})
}
