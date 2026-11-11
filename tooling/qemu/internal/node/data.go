package node

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// The host's e2fsprogs tools read an ext4 image with no mount and no privilege.
const (
	debugfs = "/sbin/debugfs"
	e2fsck  = "/sbin/e2fsck"
)

// DataSnapshot is a copy of a Node's data disk, taken while the Node runs and with the file system's
// journal replayed, that the host reads with debugfs. The live image cannot be read in place: ext4
// keeps recent metadata in the journal until it is checkpointed, so debugfs on the live file shows a
// directory without its newest entries. A reader that waits for a file takes a new snapshot at each
// poll, since a snapshot is as old as the moment it was taken and holds only what the Node's kernel
// had written by then.
type DataSnapshot struct{ img string }

// OpenData reads the ext4 image at imagePath as it is, with no journal replay.
func OpenData(imagePath string) *DataSnapshot { return &DataSnapshot{img: imagePath} }

// SnapshotData copies the Node's data disk to dst, a path on the host, and replays the journal in
// the copy.
func (n *Node) SnapshotData(dst string) (*DataSnapshot, error) {
	if out, err := exec.Command("cp", "--sparse=always", filepath.Join(n.dir, "data.img"), dst).CombinedOutput(); err != nil { //nolint:gosec // arguments built here
		return nil, fmt.Errorf("copy the data disk: %w: %s", err, out)
	}
	// e2fsck exits 1 when it corrected something, which replaying a journal counts as.
	out, err := exec.Command(e2fsck, "-fy", dst).CombinedOutput() //nolint:gosec // arguments built here
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() <= 1) {
		return nil, fmt.Errorf("e2fsck the copy: %w: %s", err, out)
	}
	return &DataSnapshot{img: dst}, nil
}

// dataPath is guestPath, a path as the Node sees it under DataMount, as debugfs names it.
func dataPath(guestPath string) (string, error) {
	rel, ok := strings.CutPrefix(path.Clean(guestPath), DataMount)
	if !ok {
		return "", fmt.Errorf("%s is not under %s", guestPath, DataMount)
	}
	return path.Clean("/" + rel), nil
}

// request runs one debugfs request, read-only, and returns its standard output.
func (s *DataSnapshot) request(req string) ([]byte, error) {
	out, err := exec.Command(debugfs, "-R", req, s.img).Output() //nolint:gosec // arguments built here
	if err != nil {
		return nil, fmt.Errorf("debugfs %q: %w", req, err)
	}
	return out, nil
}

// Read returns the contents of a file of the data disk, given as the Node sees it.
func (s *DataSnapshot) Read(guestPath string) ([]byte, error) {
	p, err := dataPath(guestPath)
	if err != nil {
		return nil, err
	}
	out, err := s.request("cat " + p)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no such file or it is empty", guestPath)
	}
	return out, nil
}

// List returns the names in a directory of the data disk, given as the Node sees it.
func (s *DataSnapshot) List(guestDir string) ([]string, error) {
	p, err := dataPath(guestDir)
	if err != nil {
		return nil, err
	}
	out, err := s.request("ls -p " + p)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		// -p prints /inode/mode/uid/gid/name/size/.
		f := strings.Split(line, "/")
		if len(f) >= 6 && f[5] != "." && f[5] != ".." && f[5] != "" {
			names = append(names, f[5])
		}
	}
	return names, nil
}

// Extract copies a file of the data disk, given as the Node sees it, to hostPath.
func (s *DataSnapshot) Extract(guestPath, hostPath string) error {
	p, err := dataPath(guestPath)
	if err != nil {
		return err
	}
	_, err = s.request(fmt.Sprintf("dump %s %s", p, hostPath))
	return err
}

// Superblock returns debugfs's account of the ext4 file system in the file at imagePath, a file on
// the host such as one Extract wrote: its magic number, features and UUID.
func Superblock(imagePath string) (string, error) {
	out, err := exec.Command(debugfs, "-R", "stats", imagePath).Output() //nolint:gosec // arguments built here
	if err != nil {
		return "", fmt.Errorf("debugfs stats %s: %w", imagePath, err)
	}
	return string(out), nil
}
