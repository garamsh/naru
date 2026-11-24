package proxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
)

// Routes holds the table the proxy forwards on and loads it again when hostd writes a new one.
type Routes struct {
	path, revisionFile string
	now                func() time.Time
	log                *slog.Logger
	current            atomic.Pointer[table]

	mu sync.Mutex
	// seen is the file last read, so a poll that finds it unchanged reads nothing.
	seen stamp
	// written is the revision the revision file holds, -1 before the first write; a write that
	// failed is tried again at the next Load.
	written int64
}

// stamp tells one version of a file from another without reading it: hostd replaces the file
// whole, so a new version is a new inode.
type stamp struct {
	ino     uint64
	size    int64
	modTime time.Time
}

// NewRoutes returns the routes read from path, the routes file, and written as a revision to
// revisionFile once loaded, with now the host's clock. It holds no table until Load first succeeds.
func NewRoutes(path, revisionFile string, now func() time.Time, log *slog.Logger) *Routes {
	return &Routes{path: path, revisionFile: revisionFile, now: now, log: log, written: -1}
}

// errStale is a table whose revision is lower than the one the proxy holds.
var errStale = errors.New("the routes file holds a lower revision than the one loaded")

// Load reads the routes file if it changed since the last read. A file that is missing,
// unreadable or does not parse, or that holds a lower revision, leaves the table as it was and is
// returned as an error. A table of the same revision is taken: hostd writes each push again with
// the time it took it, which keeps the table young. A receivedAt after now, from a clock that has
// stepped back since hostd wrote it, is taken as now, so the table lives no longer than the max
// age from its load. Each route the table drops is logged.
func (r *Routes) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("the routes file: %w", err)
	}
	st := stampOf(info)
	if st == r.seen {
		return r.writeRevision()
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("the routes file: %w", err)
	}
	r.seen = st
	t, drops, err := parseTable(raw)
	if err != nil {
		return err
	}
	for _, d := range drops {
		r.log.Warn("dropped a route", "revision", t.revision, "host", d.hostname, "err", d.err.Error())
	}
	if now := r.now(); t.receivedAt.After(now) {
		r.log.Warn("the routes file was taken after now; its age counts from now", "revision", t.revision, "received_at", t.receivedAt)
		t.receivedAt = now
	}
	if cur := r.current.Load(); cur != nil && t.revision < cur.revision {
		return fmt.Errorf("%w: %d below %d", errStale, t.revision, cur.revision)
	}
	r.current.Store(t)
	r.log.Info("loaded the routes", "revision", t.revision, "routes", len(t.routes), "received_at", t.receivedAt)
	return r.writeRevision()
}

// writeRevision writes the revision of the table loaded to the revision file, for hostd to answer
// orch with, unless the file already holds it.
func (r *Routes) writeRevision() error {
	t := r.current.Load()
	if t == nil || t.revision == r.written {
		return nil
	}
	if err := atomicfile.Write(r.revisionFile, []byte(strconv.FormatInt(t.revision, 10)+"\n"), 0o644); err != nil {
		return fmt.Errorf("the revision file: %w", err)
	}
	r.written = t.revision
	return nil
}

// Watch loads the routes file every interval until ctx ends, and logs each failure once per
// version of the file.
func (r *Routes) Watch(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	var last string
	for {
		if err := r.Load(); err != nil {
			if msg := err.Error(); msg != last {
				r.log.Warn("kept the routes it had", "err", err)
				last = msg
			}
		} else {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (r *Routes) table() *table { return r.current.Load() }

func stampOf(info fs.FileInfo) stamp {
	st := stamp{size: info.Size(), modTime: info.ModTime()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st
}
