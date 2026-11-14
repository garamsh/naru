package console_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/console"
)

// gatedDevice is a console device that takes no write until it is opened.
type gatedDevice struct {
	entered chan struct{} // receives once for each write that starts
	release chan struct{}
	buf     lockedBuffer
	once    sync.Once
}

func newGatedDevice() *gatedDevice {
	return &gatedDevice{entered: make(chan struct{}, 1024), release: make(chan struct{})}
}

func (g *gatedDevice) Write(p []byte) (int, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.buf.Write(p)
}

func (g *gatedDevice) open() { g.once.Do(func() { close(g.release) }) }

// blockOn writes a line and waits until the device holds it, so the queue behind it is empty.
func blockOn(t *testing.T, c *console.Console, g *gatedDevice) {
	t.Helper()
	_, _ = c.Write([]byte("first\n"))
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the device was not written")
	}
}

func TestConsole_AWriteReturnsWhileTheDeviceIsBlocked(t *testing.T) {
	g := newGatedDevice()
	defer g.open()
	c := console.New(g, console.KeptBytes)
	blockOn(t, c, g)

	start := time.Now()
	for i := 0; i < 10; i++ {
		_, _ = c.Source("svc").Write([]byte(strings.Repeat("x", console.QueueBytes/4) + "\n"))
	}

	assert.Less(t, time.Since(start), 100*time.Millisecond, "writes past the queue's bound must not wait either")
}

func TestConsole_ReprintReturnsWhileTheDeviceIsBlockedAndASecondOneIsSkippedAndCounted(t *testing.T) {
	g := newGatedDevice()
	defer g.open()
	c := console.New(g, console.KeptBytes)
	blockOn(t, c, g)

	start := time.Now()
	c.Reprint()
	c.Reprint()
	assert.Less(t, time.Since(start), 100*time.Millisecond)
	g.open()
	flush(t, c)

	seen := g.buf.String()
	assert.Equal(t, 1, strings.Count(seen, "NARU-LOG-BEGIN"), "the second re-print was not queued")
	assert.Contains(t, seen, "NARU-LOG-BEGIN seq=1 lines=1 dropped=0\nfirst\nNARU-LOG-END seq=1\n")
	assert.Contains(t, seen, "NARU-CONSOLE dropped=0 skipped=1\n")

	reprint(t, c)
	assert.Contains(t, g.buf.String(), "NARU-LOG-END seq=2\n", "a skipped re-print does not use a sequence number")
}

func TestConsole_LinesKeepsEveryLineWhileTheDeviceDrops(t *testing.T) {
	g := newGatedDevice()
	defer g.open()
	c := console.New(g, console.KeptBytes)
	blockOn(t, c, g)
	svc := c.Source("svc")
	const lines = 100
	for i := 0; i < lines; i++ {
		_, _ = fmt.Fprintf(svc, "%s %d\n", strings.Repeat("y", 4000), i)
	}

	got, _, _ := c.Lines("svc", 0)
	g.open()
	flush(t, c)

	require.Len(t, got, lines)
	assert.True(t, strings.HasSuffix(got[lines-1], fmt.Sprintf(" %d", lines-1)))
	assert.NotContains(t, g.buf.String(), fmt.Sprintf(" %d\n", lines-1), "the control: the device did lose lines")
}

func TestConsole_TheDropCountReachesTheDeviceOnceItTakesWritesAgain(t *testing.T) {
	g := newGatedDevice()
	defer g.open()
	c := console.New(g, console.KeptBytes)
	blockOn(t, c, g)
	_, _ = c.Write([]byte(strings.Repeat("a", console.QueueBytes-1) + "\n")) // fills the queue
	_, _ = c.Write([]byte(strings.Repeat("b", 100)))                         // does not fit
	_, _ = c.Write([]byte(strings.Repeat("c", 50) + "\n"))                   // does not fit

	g.open()
	flush(t, c)

	seen := g.buf.String()
	assert.Contains(t, seen, "NARU-CONSOLE dropped=151 skipped=0\n")
	assert.NotContains(t, seen, "bbbb")
	assert.Equal(t, 1, strings.Count(seen, "NARU-CONSOLE"))
}

func TestConsole_NothingIsReportedWhenNothingWasLost(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Write([]byte("one\n"))
	reprint(t, c)
	reprint(t, c)

	assert.NotContains(t, out.String(), "NARU-CONSOLE")
}

func TestConsole_AReportStartsOnALineOfItsOwn(t *testing.T) {
	g := newGatedDevice()
	defer g.open()
	c := console.New(g, console.KeptBytes)
	_, _ = c.Write([]byte("open"))
	<-g.entered
	_, _ = c.Write([]byte(strings.Repeat("a", console.QueueBytes)))
	_, _ = c.Write([]byte("lost"))

	g.open()
	flush(t, c)

	assert.Contains(t, g.buf.String(), "\nNARU-CONSOLE dropped=4 skipped=0\n")
}
