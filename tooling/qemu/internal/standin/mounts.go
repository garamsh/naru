package standin

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	Dir     string
	FSType  string
	Source  string
	Options string
}

// mountOf returns the mount whose mount point is dir, read from mountinfo
// (proc(5)): the fields before the lone "-" give the mount point and its
// options, the ones after it the file system type and the source.
func mountOf(mountinfo io.Reader, dir string) (Mount, error) {
	sc := bufio.NewScanner(mountinfo)
	var found *Mount
	for sc.Scan() {
		before, after, ok := strings.Cut(sc.Text(), " - ")
		pre, post := strings.Fields(before), strings.Fields(after)
		if !ok || len(pre) < 6 || len(post) < 3 || pre[4] != dir {
			continue
		}
		// The last mount at a mount point is the visible one.
		found = &Mount{Dir: dir, FSType: post[0], Source: post[1], Options: pre[5] + "," + post[2]}
	}
	if err := sc.Err(); err != nil {
		return Mount{}, err
	}
	if found == nil {
		return Mount{}, fmt.Errorf("%s is not a mount point", dir)
	}
	return *found, nil
}
