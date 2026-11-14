// Package console is the Node's serial console as init writes it: a writer that passes every
// line through and keeps the lines, and re-prints all of them at a fixed interval, so a reader
// that attaches after boot still sees the whole log. No writer waits on the device: one
// goroutine writes it from a bounded queue.
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
	// ReprintBytes bounds the bytes of lines in one re-print: the newest lines that fit. The
	// serial console costs the guest time for every byte written to it (ADR 0036), so a
	// re-print of everything kept would stall the Node for as long as it takes to write it.
	ReprintBytes = 16 << 10
	// BlockPiece is the most bytes of a re-print written to the device at once, and BlockGap is
	// the pause after each piece but the last. A Node does not run while the device takes a
	// write, so a block written whole stalls it for as long as the block is large (ADR 0036);
	// in pieces each stall is a piece long and the Node runs between them.
	BlockPiece = 2 << 10
	BlockGap   = 50 * time.Millisecond
	// MaxLine is the longest line kept; a longer one is kept as several.
	MaxLine = 4096
	// QueueBytes bounds the live output waiting for the device. A write that would pass it is
	// not written to the device, though it is kept. A re-print block is not counted: at most
	// one is queued or being written at a time.
	QueueBytes = 256 << 10
)

// The markers that open and close a re-print.
const (
	// BeginFormat takes the sequence number, the number of lines in the block and the number
	// of lines since boot that the block does not carry: dropped from the kept lines, or older
	// than the newest ReprintBytes.
	BeginFormat = "NARU-LOG-BEGIN seq=%d lines=%d dropped=%d\n"
	// EndFormat takes the sequence number.
	EndFormat = "NARU-LOG-END seq=%d\n"
	// ReportFormat takes the bytes of live output not written to the device and the number of
	// re-prints skipped since the last report. It is a line of its own, written between writes
	// and never inside a block, once the device has taken a write after the loss.
	ReportFormat = "NARU-CONSOLE dropped=%d skipped=%d\n"
)

// Console passes writes to out and keeps the complete lines written, each with the source
// that wrote it. It is safe for concurrent use. Writes reach out in order through one
// goroutine, and a re-print block is written in pieces with nothing else between them, so it is
// contiguous. Nothing that calls
// Console waits on out; the mutex is never held while out is written.
type Console struct {
	out   io.Writer
	limit int

	mu       sync.Mutex
	lines    []entry
	kept     int
	dropped  int
	own      stream // what Write receives
	sequence int
	last     uint64        // the number of the last line kept
	changed  chan struct{} // closed when the next line is kept; nil while nobody waits

	queue   []item
	queued  int           // bytes of live items in queue
	block   bool          // a re-print block is in queue or being written
	lost    int           // bytes of live output not queued since the last report
	skipped int           // re-prints skipped since the last report
	writing bool          // the writer goroutine is running
	drained chan struct{} // closed when the writer goroutine ends; nil while nobody waits
}

// item is one write to out: live output, a re-print block, or a report. A block and a report
// start on a line of their own.
type item struct {
	p       []byte
	block   bool
	ownLine bool
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
	c.keep(s, p)
	if c.queued+len(p) > QueueBytes {
		c.lost += len(p)
	} else {
		c.queued += len(p)
		c.enqueue(item{p: bytes.Clone(p)})
	}
	return len(p), nil
}

// enqueue adds it to the queue and starts the writer goroutine if it is not running. The caller
// holds mu.
func (c *Console) enqueue(it item) {
	c.queue = append(c.queue, it)
	if !c.writing {
		c.writing = true
		go c.drain()
	}
}

// drain writes to out until nothing is queued and nothing is unreported, then ends. Only this
// goroutine touches out. A loss is reported as soon as the write in flight returns, which is
// when the device has room.
func (c *Console) drain() {
	midLine := false // the last byte written to out was not a newline
	for {
		c.mu.Lock()
		var it item
		switch {
		case c.lost > 0 || c.skipped > 0:
			it = item{p: []byte(fmt.Sprintf(ReportFormat, c.lost, c.skipped)), ownLine: true}
			c.lost, c.skipped = 0, 0
		case len(c.queue) > 0:
			it = c.queue[0]
			c.queue = c.queue[1:]
			if !it.block {
				c.queued -= len(it.p)
			}
		default:
			c.writing = false
			if c.drained != nil {
				close(c.drained)
				c.drained = nil
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		p := it.p
		if midLine && it.ownLine {
			p = append([]byte{'\n'}, p...)
		}
		if len(p) > 0 {
			// A console that cannot be written to has nowhere to report it.
			if it.block {
				for len(p) > BlockPiece {
					_, _ = c.out.Write(p[:BlockPiece])
					p = p[BlockPiece:]
					time.Sleep(BlockGap)
				}
			}
			_, _ = c.out.Write(p)
			midLine = p[len(p)-1] != '\n'
		}

		if it.block {
			c.mu.Lock()
			c.block = false
			c.mu.Unlock()
		}
	}
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

// Reprint queues one block for the output: the begin marker, the newest kept lines that fit in
// ReprintBytes, the end marker.
// It copies the kept lines and returns without waiting for the output. While the previous
// block is still queued or being written it queues nothing and counts the skip, and the
// sequence number is not used.
func (c *Console) Reprint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.block {
		c.skipped++
		return
	}
	c.sequence++
	first, size := len(c.lines), 0
	for first > 0 && size+len(c.lines[first-1].text)+1 <= ReprintBytes {
		first--
		size += len(c.lines[first].text) + 1
	}
	var block bytes.Buffer
	fmt.Fprintf(&block, BeginFormat, c.sequence, len(c.lines)-first, c.dropped+first)
	for _, l := range c.lines[first:] {
		block.Write(l.text)
		block.WriteByte('\n')
	}
	fmt.Fprintf(&block, EndFormat, c.sequence)
	c.block = true
	c.enqueue(item{p: block.Bytes(), block: true, ownLine: true})
}

// Flush returns when everything queued for the output has been written, or when ctx ends.
func (c *Console) Flush(ctx context.Context) error {
	c.mu.Lock()
	if !c.writing {
		c.mu.Unlock()
		return nil
	}
	if c.drained == nil {
		c.drained = make(chan struct{})
	}
	done := c.drained
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
			c.Reprint()
		}
	}
}
