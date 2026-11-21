package volumes

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// mountEntry is a line of /proc/self/mountinfo.
type mountEntry struct {
	point, fsType, source string
	// superOptions are the file system's own options, among them prjquota.
	superOptions []string
}

// mountInfo parses the lines of /proc/self/mountinfo.
func mountInfo(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		before, after, ok := strings.Cut(sc.Text(), " - ")
		fields := strings.Fields(before)
		rest := strings.Fields(after)
		if !ok || len(fields) < 5 || len(rest) < 3 {
			return nil, fmt.Errorf("mountinfo line %q has no mount point or file system", sc.Text())
		}
		out = append(out, mountEntry{point: unescape(fields[4]), fsType: rest[0], source: unescape(rest[1]), superOptions: strings.Split(rest[2], ",")})
	}
	return out, sc.Err()
}

// unescape reads the octal escapes (\040 for a space) mountinfo writes in a path.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		var c int
		if s[i] == '\\' && i+4 <= len(s) {
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &c); err == nil {
				b.WriteByte(byte(c))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// holding returns the mount that holds path: the one with the longest mount point that contains it.
func holding(entries []mountEntry, path string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for _, e := range entries {
		if (path == e.point || strings.HasPrefix(path, strings.TrimSuffix(e.point, "/")+"/") || e.point == "/") && (!found || len(e.point) >= len(best.point)) {
			best, found = e, true
		}
	}
	return best, found
}

func readMountInfo() ([]mountEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return mountInfo(f)
}

// isMountPoint reports whether path is exactly a mount point.
func isMountPoint(entries []mountEntry, path string) bool {
	path = filepath.Clean(path)
	for _, e := range entries {
		if e.point == path {
			return true
		}
	}
	return false
}
