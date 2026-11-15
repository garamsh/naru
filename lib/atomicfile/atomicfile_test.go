package atomicfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/atomicfile"
)

func TestWrite_AReaderSeesTheOldFileOrTheNewAndNeverAPartOfEither(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	old, next := bytes.Repeat([]byte("o"), 1<<20), bytes.Repeat([]byte("n"), 3<<20)
	require.NoError(t, atomicfile.Write(path, old, 0o600))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var partial int
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil || !(bytes.Equal(b, old) || bytes.Equal(b, next)) {
				partial++
			}
		}
	})

	for range 30 {
		require.NoError(t, atomicfile.Write(path, next, 0o600))
		require.NoError(t, atomicfile.Write(path, old, 0o600))
	}
	close(stop)
	wg.Wait()

	assert.Zero(t, partial, "reads that saw a partial or no file")
}

func TestWrite_TheFileHasTheModeAndNoTemporaryFileIsLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	require.NoError(t, atomicfile.Write(path, []byte("x"), 0o640))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestWrite_AFailureLeavesTheOldFileAndNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, atomicfile.Write(path, []byte("old"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d"), 0o700))

	err := atomicfile.Write(filepath.Join(dir, "d"), []byte("new"), 0o600)

	require.Error(t, err, "a directory is not replaced by a file")
	b, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "old", string(b))
	entries, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	assert.Len(t, entries, 2, "f and d only")
}

func TestWrite_OwnerIsAppliedToTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")

	require.NoError(t, atomicfile.Write(path, []byte("x"), 0o600, atomicfile.Owner(os.Getuid(), os.Getgid())))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(os.Getuid()), st.Uid)
	err = atomicfile.Write(path, []byte("y"), 0o600, atomicfile.Owner(os.Getuid()+1, os.Getgid()))
	if os.Geteuid() != 0 {
		assert.Error(t, err, "the control: a uid the caller may not give the file is refused, so Owner is applied")
	}
}
