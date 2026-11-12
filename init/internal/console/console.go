// Package console is the Node's serial console as init writes it: a writer that passes every
// line through and keeps the lines, and re-prints all of them at a fixed interval, so a reader
// that attaches after boot still sees the whole log.
package console

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// Interval is the time between two re-prints.
	Interval = 30 * time.Second
	// KeptBytes bounds the bytes of lines kept. Past it the oldest lines are dropped, and the
	// begin marker of each re-print says how many.
	KeptBytes = 1 << 20
	// MaxLine is the longest line kept; a longer one is kept as several.
	MaxLine = 4096
)

// The markers that open and close a re-print.
const (
	// BeginFormat takes the sequence number, the number of lines in the block and the number
	// of lines dropped since boot.
	BeginFormat = "NARU-LOG-BEGIN seq=%d lines=%d dropped=%d\n"
	// EndFormat takes the sequence number.
	EndFormat = "NARU-LOG-END seq=%d\n"
)

// Console passes writes to out and keeps the complete lines written, each with the source
// that wrote it. It is safe for concurrent use; a re-print holds off writes until it is done,
// so its block is contiguous.
type Console struct {
	out   io.Writer
	limit int

	mu       sync.Mutex
	lines    []entry
	kept     int
	dropped  int
	own      stream // what Write receives
	midLine  bool   // the last byte written to out was not a newline
	sequence int
	last     uint64        // the number of the last line kept
	changed  chan struct{} // closed when the next line is kept; nil while nobody waits
}

// entry is a kept line and the source that wrote it, which is empty for Write.
type entry struct {
	seq    uint64
	source string
	text   []byte
}

// stream is one writer's pending line.
type stream struct {
	source  string
	partial []byte
}

// New returns a Console that writes to out and keeps at most limit bytes of lines.
func New(out io.Writer, limit int) *Console {
	return &Console{out: out, limit: limit}
}

// Write writes p to the output at once and keeps the lines it completes.
func (c *Console) Write(p []byte) (int, error) {
	return c.write(&c.own, p)
}

// Source returns a writer that behaves as Write and tags the lines it completes with source.
// Each writer holds its own pending line, so two sources never share one.
func (c *Console) Source(source string) io.Writer {
	return &sourceWriter{c: c, s: stream{source: source}}
}

type sourceWriter struct {
	c *Console
	s stream
}

func (w *sourceWriter) Write(p []byte) (int, error) { return w.c.write(&w.s, p) }

func (c *Console) write(s *stream, p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.out.Write(p)
	if n > 0 {
		c.midLine = p[n-1] != '\n'
	}
	c.keep(s, p)
	return len(p), err
}

// keep appends p to the stream's pending line and keeps each line it completes.
func (c *Console) keep(s *stream, p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.partial = append(s.partial, p...)
			p = nil
			c.addFull(s)
			continue
		}
		s.partial = append(s.partial, p[:i]...)
		p = p[i+1:]
		c.addFull(s)
		c.add(s.source, s.partial)
		s.partial = s.partial[:0]
	}
}

// addFull keeps MaxLine-byte pieces of the pending line while it is longer than that.
func (c *Console) addFull(s *stream) {
	for len(s.partial) > MaxLine {
		c.add(s.source, s.partial[:MaxLine])
		s.partial = append(s.partial[:0], s.partial[MaxLine:]...)
	}
}

func (c *Console) add(source string, line []byte) {
	c.last++
	c.lines = append(c.lines, entry{seq: c.last, source: source, text: bytes.Clone(line)})
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
	c.kept += len(line) + 1
	drop := 0
	for c.kept > c.limit && drop < len(c.lines) {
		c.kept -= len(c.lines[drop].text) + 1
		drop++
	}
	if drop > 0 {
		c.dropped += drop
		c.lines = append(c.lines[:0], c.lines[drop:]...)
	}
}

// Reprint writes one block: the begin marker, every kept line, the end marker. It starts on a
// fresh line when the console is mid-line.
func (c *Console) Reprint() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequence++
	var block bytes.Buffer
	if c.midLine {
		block.WriteByte('\n')
	}
	fmt.Fprintf(&block, BeginFormat, c.sequence, len(c.lines), c.dropped)
	for _, l := range c.lines {
		block.Write(l.text)
		block.WriteByte('\n')
	}
	fmt.Fprintf(&block, EndFormat, c.sequence)
	_, err := c.out.Write(block.Bytes())
	c.midLine = false
	return err
}

// Lines returns the kept lines of source numbered after the given number, the number of the
// last line kept so far, which a later call passes as after, and a channel that closes when
// another line is kept. A line number is never reused; lines dropped for the bound are not
// returned.
func (c *Console) Lines(source string, after uint64) (lines []string, last uint64, changed <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if l.seq > after && l.source == source {
			lines = append(lines, string(l.text))
		}
	}
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return lines, c.last, c.changed
}

// Run re-prints on every value of tick until ctx ends.
func (c *Console) Run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			// A console that cannot be written to has nowhere to report it.
			_ = c.Reprint()
		}
	}
}
