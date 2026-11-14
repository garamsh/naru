package linux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

// otherGroup returns a group of the test's user that is not its primary one, so a chown to it
// is seen on the file, or skips when the user has none (chown to anything else needs root).
func otherGroup(t *testing.T) int {
	t.Helper()
	groups, err := os.Getgroups()
	require.NoError(t, err)
	for _, g := range groups {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("the user has no group besides its primary one, so a chown is not visible without root")
	return 0
}

func gidOf(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	return int(info.Sys().(*syscall.Stat_t).Gid)
}

func TestCredential(t *testing.T) {
	assert.Nil(t, credential(nil), "a service with no user runs as init")

	c := credential(&config.User{UID: 6002, GID: 6002, Groups: []uint32{6001}})

	assert.Equal(t, uint32(6002), c.Uid)
	assert.Equal(t, uint32(6002), c.Gid)
	assert.Equal(t, []uint32{6001}, c.Groups)
	assert.NotNil(t, credential(&config.User{UID: 1, GID: 1}).Groups, "no groups is an empty list, which drops init's, not nil")
}

func TestPrepareDirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a/b")

	require.NoError(t, prepareDirs([]config.Dir{{Path: dir, Mode: "0700", UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}}))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, prepareDirs([]config.Dir{{Path: dir, Mode: "0700", UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}}))
	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "a directory that exists is brought to the mode")

	file := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	assert.Error(t, prepareDirs([]config.Dir{{Path: file}}), "a file is not a directory")

	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(dir, link))
	assert.Error(t, prepareDirs([]config.Dir{{Path: link}}), "a symbolic link is not followed")
}

func TestPrepareDirs_Owner(t *testing.T) {
	gid := otherGroup(t)
	dir := filepath.Join(t.TempDir(), "d")

	require.NoError(t, prepareDirs([]config.Dir{{Path: dir, UID: uint32(os.Getuid()), GID: uint32(gid)}}))

	assert.Equal(t, gid, gidOf(t, dir))
}

func TestWriteFilesAndReplaceFile_Owner(t *testing.T) {
	gid := otherGroup(t)
	h := &Host{}
	dir := t.TempDir()
	owner := &config.Owner{UID: uint32(os.Getuid()), GID: uint32(gid)}

	require.NoError(t, h.WriteFiles([]config.File{{Path: filepath.Join(dir, "a"), Mode: "0600", Content: "x", Owner: owner}}))
	assert.Equal(t, gid, gidOf(t, filepath.Join(dir, "a")))

	require.NoError(t, h.ReplaceFile(config.File{Path: filepath.Join(dir, "b"), Mode: "0600", Content: "x", Owner: owner}))
	assert.Equal(t, gid, gidOf(t, filepath.Join(dir, "b")))
	info, err := os.Stat(filepath.Join(dir, "b"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestRunSetup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reaper := NewReaper()
	go reaper.Run(ctx)
	l := &Launcher{reaper: reaper}
	marker := filepath.Join(t.TempDir(), "marker")
	sh := func(script string) func([]string) *exec.Cmd {
		return func(args []string) *exec.Cmd {
			return exec.Command("/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
		}
	}

	t.Run("runs, with its output on the console, and creates what the service needs", func(t *testing.T) {
		var out bytes.Buffer
		svc := config.Service{Name: "s", Setup: []config.Setup{{Args: []string{"hello"}, Unless: []string{marker}}}}

		err := l.runSetup(svc, sh(`echo "said $1"; echo err >&2; : > `+marker), &out)

		require.NoError(t, err)
		assert.Contains(t, out.String(), "said hello")
		assert.Contains(t, out.String(), "err")
		assert.FileExists(t, marker)
	})

	t.Run("is skipped when the file it makes is there", func(t *testing.T) {
		var out bytes.Buffer
		svc := config.Service{Name: "s", Setup: []config.Setup{{Args: []string{"x"}, Unless: []string{marker}}}}

		err := l.runSetup(svc, sh(`echo ran`), &out)

		require.NoError(t, err)
		assert.Empty(t, out.String())
	})

	t.Run("runs each setup in order and skips one when any of its paths exists", func(t *testing.T) {
		dir := t.TempDir()
		first, second, third := filepath.Join(dir, "first"), filepath.Join(dir, "second"), filepath.Join(dir, "third")
		svc := config.Service{Name: "s", Setup: []config.Setup{
			{Args: []string{first}, Unless: []string{first}},
			{Args: []string{second}, Unless: []string{second, first}}, // first is made by the step before: skipped
			{Args: []string{third}, Unless: []string{filepath.Join(dir, "none"), third}},
		}}

		err := l.runSetup(svc, sh(`: > "$1"`), &bytes.Buffer{})

		require.NoError(t, err)
		assert.FileExists(t, first)
		assert.NoFileExists(t, second, "one path of its Unless exists")
		assert.FileExists(t, third)
	})

	t.Run("stops at the first setup that fails", func(t *testing.T) {
		after := filepath.Join(t.TempDir(), "after")
		svc := config.Service{Name: "s", Setup: []config.Setup{{Args: []string{"x"}}, {Args: []string{after}}}}

		err := l.runSetup(svc, sh(`case "$1" in x) exit 1;; *) : > "$1";; esac`), &bytes.Buffer{})

		require.Error(t, err)
		assert.NoFileExists(t, after)
	})

	t.Run("fails the start when it exits non-zero", func(t *testing.T) {
		svc := config.Service{Name: "s", Setup: []config.Setup{{Args: []string{"x"}, Unless: []string{marker + ".none"}}}}

		err := l.runSetup(svc, sh(`exit 3`), &bytes.Buffer{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "exit status 3")
	})

	t.Run("fails when the binary cannot start", func(t *testing.T) {
		svc := config.Service{Name: "s", Setup: []config.Setup{{Args: []string{"x"}}}}

		err := l.runSetup(svc, func([]string) *exec.Cmd { return exec.Command("/nonexistent/bin") }, &bytes.Buffer{})

		assert.Error(t, err)
	})

	t.Run("is nothing for a service without one", func(t *testing.T) {
		assert.NoError(t, l.runSetup(config.Service{Name: "s"}, nil, &bytes.Buffer{}))
	})
}
