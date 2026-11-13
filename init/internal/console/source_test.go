package console_test

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/init/internal/console"
)

func TestConsole_LinesReturnsOnlyTheLinesOfOneSource(t *testing.T) {
	c := console.New(io.Discard, console.KeptBytes)
	a, b := c.Source("a"), c.Source("b")
	_, _ = a.Write([]byte("a1\n"))
	_, _ = b.Write([]byte("b1\n"))
	_, _ = c.Write([]byte("init says\n"))
	_, _ = a.Write([]byte("a2\n"))

	lines, _, _ := c.Lines("a", 0)
	other, _, _ := c.Lines("b", 0)

	assert.Equal(t, []string{"a1", "a2"}, lines)
	assert.Equal(t, []string{"b1"}, other, "the control: another source's lines are still there")
}

func TestConsole_TwoSourcesDoNotShareAPendingLine(t *testing.T) {
	c := console.New(io.Discard, console.KeptBytes)
	a, b := c.Source("a"), c.Source("b")
	_, _ = a.Write([]byte("hel"))
	_, _ = b.Write([]byte("wor"))
	_, _ = a.Write([]byte("lo\n"))
	_, _ = b.Write([]byte("ld\n"))

	lines, _, _ := c.Lines("a", 0)
	other, _, _ := c.Lines("b", 0)

	assert.Equal(t, []string{"hello"}, lines)
	assert.Equal(t, []string{"world"}, other)
}

func TestConsole_LinesAfterANumberReturnsOnlyTheLinesKeptSince(t *testing.T) {
	c := console.New(io.Discard, console.KeptBytes)
	a := c.Source("a")
	_, _ = a.Write([]byte("one\n"))
	_, last, _ := c.Lines("a", 0)
	_, _ = a.Write([]byte("two\n"))

	lines, _, _ := c.Lines("a", last)

	assert.Equal(t, []string{"two"}, lines)
}

func TestConsole_LinesChannelClosesWhenALineIsKept(t *testing.T) {
	c := console.New(io.Discard, console.KeptBytes)
	_, _, changed := c.Lines("a", 0)
	select {
	case <-changed:
		t.Fatal("the channel closed before any line was kept")
	default:
	}

	_, _ = c.Source("a").Write([]byte("one\n"))

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the channel did not close when a line was kept")
	}
}

func TestConsole_ReprintKeepsTheBlockFormatWithTaggedLines(t *testing.T) {
	out := &lockedBuffer{}
	c := console.New(out, console.KeptBytes)
	_, _ = c.Source("a").Write([]byte("from a\n"))
	_, _ = c.Write([]byte("from init\n"))
	flush(t, c)
	out.Reset()

	reprint(t, c)

	assert.Equal(t, "NARU-LOG-BEGIN seq=1 lines=2 dropped=0\nfrom a\nfrom init\nNARU-LOG-END seq=1\n", out.String())
}
