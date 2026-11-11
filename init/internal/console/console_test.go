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

	"github.com/garamsh/naru/init/internal/console"
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

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestConsole_PassesWritesThroughAtOnce(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, console.KeptBytes)

	n, err := c.Write([]byte("one\ntwo"))

	require.NoError(t, err)
	assert.Equal(t, 7, n)
	assert.Equal(t, "one\ntwo", out.String(), "a partial line is written when it arrives")
}

func TestConsole_ReprintWritesEveryLineSinceBootBetweenNumberedMarkers(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, console.KeptBytes)
	_, _ = c.Write([]byte("boot line\nservice says\n"))
	out.Reset()

	require.NoError(t, c.Reprint())
	_, _ = c.Write([]byte("late line\n"))
	require.NoError(t, c.Reprint())

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=2 dropped=0\nboot line\nservice says\nNARU-LOG-END seq=1\n"+
		"late line\n"+
		"NARU-LOG-BEGIN seq=2 lines=3 dropped=0\nboot line\nservice says\nlate line\nNARU-LOG-END seq=2\n", out.String())
}

func TestConsole_KeepsTheLinesOfSeveralWritersAndSplitsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, console.KeptBytes)
	for _, chunk := range []string{"par", "tial one\nsec", "ond\n\nthird\n"} {
		_, _ = c.Write([]byte(chunk))
	}
	out.Reset()

	require.NoError(t, c.Reprint())

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=4 dropped=0\npartial one\nsecond\n\nthird\nNARU-LOG-END seq=1\n", out.String())
}

func TestConsole_ReprintLeavesTheLineThatIsStillOpenAndStartsOnANewLine(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, console.KeptBytes)
	_, _ = c.Write([]byte("done\nunfinished"))
	out.Reset()

	require.NoError(t, c.Reprint())
	_, _ = c.Write([]byte(" now finished\n"))
	out.Reset()
	require.NoError(t, c.Reprint())

	assert.Contains(t, out.String(), "unfinished now finished\nNARU-LOG-END seq=2\n", "the line is kept whole once it ends")
}

func TestConsole_PastTheBoundTheOldestLinesAreDroppedAndCounted(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, 30) // each "line N\n" costs 7 bytes, so four lines fit
	for i := 1; i <= 6; i++ {
		_, _ = fmt.Fprintf(c, "line %d\n", i)
	}
	out.Reset()

	require.NoError(t, c.Reprint())

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=4 dropped=2\nline 3\nline 4\nline 5\nline 6\nNARU-LOG-END seq=1\n", out.String())
}

func TestConsole_ALongLineIsKeptInPiecesNoLongerThanTheBound(t *testing.T) {
	var out bytes.Buffer
	c := console.New(&out, console.KeptBytes)
	_, _ = c.Write([]byte(strings.Repeat("x", console.MaxLine*2+10) + "\n"))
	out.Reset()

	require.NoError(t, c.Reprint())

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
	tick <- time.Time{}
	cancel()
	<-done

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
	attached := len(out.String()) // the reader attaches here: it has seen none of the lines above
	_, _ = c.Write([]byte("event 51\n"))

	require.NoError(t, c.Reprint())

	seen := out.String()[attached:]
	for i := 1; i <= 51; i++ {
		assert.Contains(t, seen, fmt.Sprintf("event %d\n", i))
	}
}
