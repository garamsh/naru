// Package modcheck keeps the modules of go.work in step: one version of each dependency two of them
// share, and the go directive of go.work in every go.mod.
package modcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/mod/modfile"

	"github.com/garamsh/naru/tooling/importcheck/internal/workspace"
)

// Skew is a dependency that two modules require at different versions.
type Skew struct {
	Path string
	// Modules maps a version to the folders of the modules requiring it.
	Modules map[string][]string
}

func (s Skew) String() string {
	versions := make([]string, 0, len(s.Modules))
	for v := range s.Modules {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	out := s.Path + " is required at"
	for _, v := range versions {
		out += fmt.Sprintf(" %s by %v;", v, s.Modules[v])
	}
	return out
}

// Result is what Check examined and what it found.
type Result struct {
	// Modules counts the modules go.work uses, and Dependencies the distinct dependencies they
	// require, indirect ones included.
	Modules, Dependencies int
	Skews                 []Skew
	// WrongGo holds the folders whose go directive is not go.work's.
	WrongGo []string
}

// Check reads the go.mod of every module go.work uses. A module of the workspace itself is not a
// dependency here: its requirement is the placeholder version its replace line resolves.
func Check(root string) (Result, error) {
	work, err := workspace.Read(root)
	if err != nil {
		return Result{}, err
	}
	files := map[string]*modfile.File{}
	own := map[string]bool{}
	for _, dir := range work.Use {
		gomod := filepath.Join(root, filepath.FromSlash(dir), "go.mod")
		b, err := os.ReadFile(gomod)
		if err != nil {
			return Result{}, err
		}
		f, err := modfile.ParseLax(gomod, b, nil)
		if err != nil {
			return Result{}, err
		}
		if f.Module == nil {
			return Result{}, fmt.Errorf("%s has no module line", gomod)
		}
		files[dir] = f
		own[f.Module.Mod.Path] = true
	}
	res := Result{Modules: len(work.Use)}
	versions := map[string]map[string][]string{}
	for _, dir := range work.Use {
		f := files[dir]
		if f.Go == nil || f.Go.Version != work.GoVersion {
			res.WrongGo = append(res.WrongGo, dir)
		}
		for _, r := range f.Require {
			if own[r.Mod.Path] {
				continue
			}
			if versions[r.Mod.Path] == nil {
				versions[r.Mod.Path] = map[string][]string{}
			}
			versions[r.Mod.Path][r.Mod.Version] = append(versions[r.Mod.Path][r.Mod.Version], dir)
		}
	}
	res.Dependencies = len(versions)
	for p, byVersion := range versions {
		if len(byVersion) > 1 {
			res.Skews = append(res.Skews, Skew{Path: p, Modules: byVersion})
		}
	}
	sort.Slice(res.Skews, func(i, j int) bool { return res.Skews[i].Path < res.Skews[j].Path })
	sort.Strings(res.WrongGo)
	return res, nil
}
