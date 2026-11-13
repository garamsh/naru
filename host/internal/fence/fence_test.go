package fence

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
)

type fakeRT struct {
	mvmd.Runtime
	mu      sync.Mutex
	last    uint64
	numbers []uint64
	fail    int
}

func (f *fakeRT) InspectDeadline(context.Context) (mvmd.Deadline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return mvmd.Deadline{LastNumber: f.last}, nil
}

func (f *fakeRT) Renew(_ context.Context, _ time.Duration, n uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("down")
	}
	f.numbers = append(f.numbers, n)
	f.last = n
	return nil
}

func (f *fakeRT) seen() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.numbers...)
}

func TestRenewer_NumbersContinueAboveMvmdsLastAndSurviveAFailure(t *testing.T) {
	f := &fakeRT{last: 41, fail: 1}
	r := &Renewer{Runtime: f, Length: time.Minute, Interval: 5 * time.Millisecond, Retry: 5 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return len(f.seen()) >= 3 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	got := f.seen()
	assert.Equal(t, uint64(42), got[0], "the first renewal is above the number mvmd holds")
	for i := 1; i < len(got); i++ {
		assert.Greater(t, got[i], got[i-1])
	}
}
