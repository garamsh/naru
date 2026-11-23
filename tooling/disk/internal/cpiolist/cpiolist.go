// Package cpiolist orders and checks the list format of the kernel's usr/gen_init_cpio, which the
// initramfs of the Host and of the Node are made from (ADR 0020, 0075). The kernel's initramfs
// unpacker creates no parent directory: an entry whose parent has not been unpacked yet fails with
// ENOENT and is skipped without a message, so a list must name every directory before what is in it.
package cpiolist

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

// ImplicitDir is the entry made for a directory that holds an entry and that the list does not name.
const ImplicitDir = "0755 0 0"

// Entry is one line of a list: the kind of node (file, dir, nod, slink, pipe, sock), the absolute path
// it makes and the whole line.
type Entry struct {
	Kind, Path, Line string
	// Number is the line's number in the list it was read from, from 1.
	Number int
}

// Parse reads a list. Blank lines and comments are dropped; every other line has a kind and an
// absolute path, which is cleaned, and the line is returned with that path.
func Parse(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: %q has no path", n, line)
		}
		if !path.IsAbs(f[1]) {
			return nil, fmt.Errorf("line %d: the path %q is not absolute", n, f[1])
		}
		p := path.Clean(f[1])
		f[1] = p
		out = append(out, Entry{Kind: f[0], Path: p, Line: strings.Join(f, " "), Number: n})
	}
	return out, sc.Err()
}

// Format writes the entries one per line.
func Format(w io.Writer, entries []Entry) error {
	for _, e := range entries {
		if _, err := fmt.Fprintln(w, e.Line); err != nil {
			return err
		}
	}
	return nil
}

// ancestors returns the directories above p, the topmost first, and none for "/" and for a path
// directly under it.
func ancestors(p string) []string {
	var up []string
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		up = append(up, d)
	}
	for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
		up[i], up[j] = up[j], up[i]
	}
	return up
}

// Order returns the entries so that every directory comes before what is in it. A directory the list
// names is emitted, as the list wrote it, just before its first entry; one it does not name is made
// with ImplicitDir. The entries keep their order otherwise, so the result is the same for the same
// input. A path named twice is refused, since the archive would hold it twice.
func Order(entries []Entry) ([]Entry, error) {
	named := map[string]Entry{}
	for _, e := range entries {
		if first, ok := named[e.Path]; ok {
			return nil, fmt.Errorf("line %d: %s is named again: line %d has it", e.Number, e.Path, first.Number)
		}
		named[e.Path] = e
	}
	var out []Entry
	done := map[string]bool{}
	emit := func(e Entry) {
		out = append(out, e)
		done[e.Path] = true
	}
	for _, e := range entries {
		for _, d := range ancestors(e.Path) {
			dir, ok := named[d]
			switch {
			case ok && dir.Kind != "dir":
				return nil, fmt.Errorf("line %d: %s is in %s, which line %d makes a %s", e.Number, e.Path, d, dir.Number, dir.Kind)
			case done[d]:
			case !ok:
				emit(Entry{Kind: "dir", Path: d, Line: "dir " + d + " " + ImplicitDir})
			default:
				emit(dir)
			}
		}
		if !done[e.Path] {
			emit(e)
		}
	}
	return out, nil
}

// Check refuses a list in which an entry's parent directory is not made by an earlier entry, naming
// the entry and its line. A parent that is made by a later entry or by none is the same refusal, since
// the unpacker skips the entry either way. Only a path directly under "/" has no parent to name.
func Check(entries []Entry) error {
	kinds := map[string]string{}
	for _, e := range entries {
		if parent := path.Dir(e.Path); parent != "/" {
			switch kinds[parent] {
			case "dir":
			case "":
				return fmt.Errorf("line %d: %s comes before its parent directory %s, which the unpacker does not create", e.Number, e.Path, parent)
			default:
				return fmt.Errorf("line %d: %s is in %s, which is a %s", e.Number, e.Path, parent, kinds[parent])
			}
		}
		kinds[e.Path] = e.Kind
	}
	return nil
}
