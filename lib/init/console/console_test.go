package console_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/console"
)

// lockedBuffer is a console a test can read while the Console under test writes it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

func (l *lockedBuffer) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Len()
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// flush waits until everything queued for the device has been written.
func flush(t *testing.T, c *console.Console) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Flush(ctx))
}

// reprint queues a block and waits until the device has it.
func reprint(t *testing.T, c *console.Console) {
	t.Helper()
	c.Reprint()
	flush(t, c)
}

func TestConsole_PassesWritesThroughAtOnce(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)

	n, err := c.Write([]byte("one\ntwo"))

	require.NoError(t, err)
	assert.Equal(t, 7, n)
	flush(t, c)
	assert.Equal(t, "one\ntwo", out.String(), "a partial line is written when it arrives")
}

func TestConsole_ReprintWritesEveryLineSinceBootBetweenNumberedMarkers(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Write([]byte("boot line\nservice says\n"))
	flush(t, c)
	out.Reset()

	reprint(t, c)
	_, _ = c.Write([]byte("late line\n"))
	reprint(t, c)

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=2 dropped=0\nboot line\nservice says\nNARU-LOG-END seq=1\n"+
		"late line\n"+
		"NARU-LOG-BEGIN seq=2 lines=3 dropped=0\nboot line\nservice says\nlate line\nNARU-LOG-END seq=2\n", out.String())
}

func TestConsole_KeepsTheLinesOfSeveralWritersAndSplitsAcrossWrites(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	for _, chunk := range []string{"par", "tial one\nsec", "ond\n\nthird\n"} {
		_, _ = c.Write([]byte(chunk))
	}
	flush(t, c)
	out.Reset()

	reprint(t, c)

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=4 dropped=0\npartial one\nsecond\n\nthird\nNARU-LOG-END seq=1\n", out.String())
}

func TestConsole_ReprintLeavesTheLineThatIsStillOpenAndStartsOnANewLine(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Write([]byte("done\nunfinished"))
	flush(t, c)
	out.Reset()

	reprint(t, c)
	_, _ = c.Write([]byte(" now finished\n"))
	flush(t, c)
	out.Reset()
	reprint(t, c)

	assert.Contains(t, out.String(), "unfinished now finished\nNARU-LOG-END seq=2\n", "the line is kept whole once it ends")
}

func TestConsole_PastTheBoundTheOldestLinesAreDroppedAndCounted(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, 30) // each "line N\n" costs 7 bytes, so four lines fit
	for i := 1; i <= 6; i++ {
		_, _ = fmt.Fprintf(c, "line %d\n", i)
	}
	flush(t, c)
	out.Reset()

	reprint(t, c)

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=4 dropped=2\nline 3\nline 4\nline 5\nline 6\nNARU-LOG-END seq=1\n", out.String())
}

func TestConsole_AReprintCarriesOnlyTheNewestLinesThatFitInReprintBytesAndCountsTheRest(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	line := strings.Repeat("x", 99) + "\n" // 100 bytes with its newline
	total := 2*console.ReprintBytes/len(line) + 1
	for i := 0; i < total; i++ {
		_, _ = c.Write([]byte(line))
	}
	flush(t, c)
	out.Reset()

	reprint(t, c)

	fit := console.ReprintBytes / len(line)
	want := fmt.Sprintf("NARU-LOG-BEGIN seq=1 lines=%d dropped=%d\n", fit, total-fit)
	assert.True(t, strings.HasPrefix(out.String(), want), "the begin marker counts what the block lacks: %q", out.String()[:60])
	assert.LessOrEqual(t, out.Len(), console.ReprintBytes+100, "the block is bounded, not the whole kept log")
	assert.True(t, strings.HasSuffix(out.String(), line+"NARU-LOG-END seq=1\n"))
}

func TestConsole_ALongLineIsKeptInPiecesNoLongerThanTheBound(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Write([]byte(strings.Repeat("x", console.MaxLine*2+10) + "\n"))
	flush(t, c)
	out.Reset()

	reprint(t, c)

	lines := strings.Split(out.String(), "\n")
	require.Len(t, lines, 6, "begin, three pieces, end and the empty string after the last newline")
	assert.Equal(t, strings.Repeat("x", console.MaxLine), lines[1])
	assert.Equal(t, strings.Repeat("x", console.MaxLine), lines[2])
	assert.Equal(t, strings.Repeat("x", 10), lines[3])
}

func TestConsole_RunReprintsOnEveryTickAndStopsWithItsContext(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Write([]byte("hello\n"))
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, tick)
		close(done)
	}()

	tick <- time.Time{}
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "NARU-LOG-END seq=1\n") }, 5*time.Second, time.Millisecond)
	tick <- time.Time{}
	cancel()
	<-done
	flush(t, c)

	assert.Equal(t, 1, strings.Count(out.String(), "NARU-LOG-END seq=1\n"))
	assert.Equal(t, 1, strings.Count(out.String(), "NARU-LOG-END seq=2\n"))
	assert.Equal(t, 0, strings.Count(out.String(), "seq=3"))
}

func TestConsole_ALateReaderSeesTheWholeLogInOneBlock(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	for i := 1; i <= 50; i++ {
		_, _ = fmt.Fprintf(c, "event %d\n", i)
	}
	flush(t, c)
	attached := len(out.String()) // the reader attaches here: it has seen none of the lines above
	_, _ = c.Write([]byte("event 51\n"))

	reprint(t, c)

	seen := out.String()[attached:]
	for i := 1; i <= 51; i++ {
		assert.Contains(t, seen, fmt.Sprintf("event %d\n", i))
	}
}
