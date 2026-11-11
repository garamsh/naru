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

// Console passes writes to out and keeps the complete lines written. It is safe for
// concurrent use; a re-print holds off writes until it is done, so its block is contiguous.
type Console struct {
	out   io.Writer
	limit int

	mu       sync.Mutex
	lines    [][]byte
	kept     int
	dropped  int
	partial  []byte
	midLine  bool // the last byte written to out was not a newline
	sequence int
}

// New returns a Console that writes to out and keeps at most limit bytes of lines.
func New(out io.Writer, limit int) *Console {
	return &Console{out: out, limit: limit}
}

// Write writes p to the output at once and keeps the lines it completes.
func (c *Console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.out.Write(p)
	if n > 0 {
		c.midLine = p[n-1] != '\n'
	}
	c.keep(p)
	return len(p), err
}

// keep appends p to the pending line and keeps each line it completes.
func (c *Console) keep(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.partial = append(c.partial, p...)
			p = nil
			c.addFull()
			continue
		}
		c.partial = append(c.partial, p[:i]...)
		p = p[i+1:]
		c.addFull()
		c.add(c.partial)
		c.partial = c.partial[:0]
	}
}

// addFull keeps MaxLine-byte pieces of the pending line while it is longer than that.
func (c *Console) addFull() {
	for len(c.partial) > MaxLine {
		c.add(c.partial[:MaxLine])
		c.partial = append(c.partial[:0], c.partial[MaxLine:]...)
	}
}

func (c *Console) add(line []byte) {
	c.lines = append(c.lines, bytes.Clone(line))
	c.kept += len(line) + 1
	drop := 0
	for c.kept > c.limit && drop < len(c.lines) {
		c.kept -= len(c.lines[drop]) + 1
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
		block.Write(l)
		block.WriteByte('\n')
	}
	fmt.Fprintf(&block, EndFormat, c.sequence)
	_, err := c.out.Write(block.Bytes())
	c.midLine = false
	return err
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
