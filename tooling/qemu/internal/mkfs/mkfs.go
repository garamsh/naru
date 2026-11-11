// Package mkfs lets a Node that has no mkfs.ext4 make ext4 file systems. The
// image holds no e2fsprogs (a static build of it is not available here), and
// mvmd runs `mkfs.ext4` for each Machine's root disk. The test therefore puts
// the host's mke2fs and the shared libraries it loads into the Node's data
// disk, and a static launcher named mkfs.ext4 beside them runs it through the
// dynamic loader that came along.
package mkfs

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Names inside the directory the launcher sits in.
const (
	Loader = "ld.so"
	Mke2fs = "mke2fs"
	LibDir = "lib"
)

// Argv is the argument vector that runs mke2fs as mkfs.ext4 would, from the
// launcher's directory dir.
func Argv(dir string, args []string) []string {
	argv := []string{
		filepath.Join(dir, Loader), "--library-path", filepath.Join(dir, LibDir),
		filepath.Join(dir, Mke2fs), "-t", "ext4",
	}
	return append(argv, args...)
}

// Libraries returns the paths of the shared objects the dynamic executable
// loads, and the loader, from the output of ldd.
func Libraries(ldd string) (loader string, libs []string, err error) {
	sc := bufio.NewScanner(strings.NewReader(ldd))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		switch {
		case len(fields) >= 3 && fields[1] == "=>" && strings.HasPrefix(fields[2], "/"):
			libs = append(libs, fields[2])
		case len(fields) >= 1 && strings.HasPrefix(fields[0], "/") && strings.Contains(fields[0], "/ld-linux"):
			loader = fields[0]
		}
	}
	if loader == "" || len(libs) == 0 {
		return "", nil, fmt.Errorf("ldd output names no loader or no libraries: %q", ldd)
	}
	return loader, libs, sc.Err()
}

// HostMke2fs finds the host's mke2fs and the files it needs: the executable,
// the loader and the libraries.
func HostMke2fs() (exe, loader string, libs []string, err error) {
	exe, err = exec.LookPath("mke2fs")
	if err != nil {
		for _, p := range []string{"/sbin/mke2fs", "/usr/sbin/mke2fs"} {
			if _, e := os.Stat(p); e == nil {
				exe, err = p, nil
				break
			}
		}
		if err != nil {
			return "", "", nil, err
		}
	}
	var out bytes.Buffer
	cmd := exec.Command("ldd", exe) //nolint:gosec // the path was just found
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", "", nil, fmt.Errorf("ldd %s: %w", exe, err)
	}
	loader, libs, err = Libraries(out.String())
	return exe, loader, libs, err
}
