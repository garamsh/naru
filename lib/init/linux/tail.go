package linux

import (
	"bytes"
	"io"
	"os"
	"time"
)

// tailInterval is how often a followed log file is read for new lines.
const tailInterval = 500 * time.Millisecond

// size returns the length of the file at path, and 0 for one that does not exist.
func size(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// follow writes to w every complete line the file at path holds after its first offset bytes,
// until stop closes, and reads the file one last time then. A file that does not exist yet is
// followed once it does. It runs in the caller's goroutine.
func follow(path string, offset int64, w io.Writer, stop <-chan struct{}) {
	var partial []byte
	read := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil || fi.Size() < offset {
			offset = 0
		}
		b, err := io.ReadAll(io.NewSectionReader(f, offset, 1<<20))
		if err != nil {
			return
		}
		offset += int64(len(b))
		partial = append(partial, b...)
		if i := bytes.LastIndexByte(partial, '\n'); i >= 0 {
			_, _ = w.Write(partial[:i+1])
			partial = append([]byte(nil), partial[i+1:]...)
		}
	}
	tick := time.NewTicker(tailInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			read()
		case <-stop:
			read()
			return
		}
	}
}
