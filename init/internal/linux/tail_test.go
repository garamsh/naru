package linux

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestFollow_WritesTheLinesAppendedAfterTheCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ecr-login.log")
	appendTo(t, path, "from an earlier run\n")
	var out syncBuffer
	stop := make(chan struct{})
	done := make(chan struct{})
	from := size(path)
	go func() { follow(path, from, &out, stop); close(done) }()

	appendTo(t, path, "level=debug msg=one\nlevel=err")
	assert.Eventually(t, func() bool { return out.String() == "level=debug msg=one\n" }, 5*time.Second, 10*time.Millisecond,
		"a complete line is written and the partial one waits")
	appendTo(t, path, "or msg=two\n")
	assert.Eventually(t, func() bool { return out.String() == "level=debug msg=one\nlevel=error msg=two\n" }, 5*time.Second, 10*time.Millisecond)

	appendTo(t, path, "last\n")
	close(stop)
	<-done
	assert.Equal(t, "level=debug msg=one\nlevel=error msg=two\nlast\n", out.String(), "the last read happens when it stops")
}

func TestFollow_ReadsAFileThatAppearsLaterFromItsFirstByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ecr-login.log")
	var out syncBuffer
	stop := make(chan struct{})
	done := make(chan struct{})
	from := size(path)
	go func() { follow(path, from, &out, stop); close(done) }()

	appendTo(t, path, "first\n")
	close(stop)
	<-done

	assert.Equal(t, "first\n", out.String())
}
