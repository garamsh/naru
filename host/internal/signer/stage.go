package signer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/lib/atomicfile"
)

const (
	stageDir    = ".staging"
	stageCommit = "COMMIT" // lists the staged files; its presence is the commit point
)

// stagedFile is a file commitFiles puts in a directory.
type stagedFile struct {
	name string
	data []byte
}

// stageHook is called before each step of commitFiles with the step's name, and a non-nil error
// it returns stops the commit there, as a crash would. Nil outside a test.
var stageHook func(step string) error

func hook(step string) error {
	if stageHook == nil {
		return nil
	}
	return stageHook(step)
}

// commitFiles puts every file in dir so that a crash at any point leaves dir as it was or holds
// all of them. It writes the files into a staging directory, writes the list of them last, which
// is the commit, and then renames each into place. recoverStaging finishes a commit a crash
// stopped, or discards one the crash stopped before the list.
func commitFiles(dir string, files []stagedFile) error {
	stage := filepath.Join(dir, stageDir)
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if err := hook("write " + f.name); err != nil {
			return err
		}
		if err := atomicfile.Write(filepath.Join(stage, f.name), f.data, 0o600); err != nil {
			return err
		}
		names = append(names, f.name)
	}
	if err := hook("commit"); err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(stage, stageCommit), []byte(strings.Join(names, "\n")), 0o600); err != nil {
		return err
	}
	return rollForward(dir, stage, names)
}

// recoverStaging finishes or discards the commit a crash stopped in dir.
func recoverStaging(dir string) error {
	stage := filepath.Join(dir, stageDir)
	list, err := os.ReadFile(filepath.Join(stage, stageCommit))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.RemoveAll(stage)
	case err != nil:
		return err
	}
	return rollForward(dir, stage, strings.Split(string(list), "\n"))
}

func rollForward(dir, stage string, names []string) error {
	for _, n := range names {
		if err := hook("rename " + n); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(stage, n), filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("commit %s: %w", n, err)
		}
	}
	if err := atomicfile.SyncDir(dir); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}
