package console_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/console"
)

// stampedWrites records each write to the device with the time it was made.
type stampedWrites struct {
	mu sync.Mutex
	n  []int
	at []time.Time
	b  bytes.Buffer
}

func (s *stampedWrites) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = append(s.n, len(p))
	s.at = append(s.at, time.Now())
	return s.b.Write(p)
}

func TestConsole_AReprintReachesTheDeviceInPiecesWithAPauseBetweenThemAndNothingInside(t *testing.T) {
	out := &stampedWrites{}
	c := console.New(out, console.KeptBytes)
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 2*console.ReprintBytes/len(line); i++ {
		_, _ = c.Write([]byte(line))
	}
	flush(t, c)
	out.mu.Lock()
	out.n, out.at = nil, nil
	out.b.Reset()
	out.mu.Unlock()

	c.Reprint()
	_, _ = c.Write([]byte("live\n")) // written while the block is: it comes after the block
	flush(t, c)

	out.mu.Lock()
	defer out.mu.Unlock()
	require.Greater(t, len(out.n), 4, "a block of ReprintBytes is several pieces")
	last := len(out.n) - 1
	for i, n := range out.n[:last] {
		assert.LessOrEqual(t, n, console.BlockPiece, "write %d", i)
		if i > 0 {
			assert.GreaterOrEqual(t, out.at[i].Sub(out.at[i-1]), console.BlockGap, "pause before write %d", i)
		}
	}
	got := out.b.String()
	assert.Equal(t, 1, strings.Count(got, "NARU-LOG-END seq=1\n"))
	assert.True(t, strings.HasSuffix(got, "NARU-LOG-END seq=1\nlive\n"), "the live line follows the whole block")
}
