package timesync_test

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

	"github.com/garamsh/naru/lib/init/timesync"
)

var start = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a clock the test advances, that records how it is moved. Every After is handed
// to the test, which decides when it fires.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	stepped []time.Duration
	slewed  []time.Duration
	err     error
	timers  chan timer
}

type timer struct {
	wait time.Duration
	fire chan time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: start, timers: make(chan timer, 8)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers <- timer{d, ch}
	return ch
}

func (c *fakeClock) Step(d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stepped = append(c.stepped, d)
	return c.err
}

func (c *fakeClock) Slew(d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slewed = append(c.slewed, d)
	return c.err
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) moves() (stepped, slewed []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.stepped...), append([]time.Duration(nil), c.slewed...)
}

// fakeServers answers each server address with what the test set for it.
type fakeServers struct {
	mu      sync.Mutex
	answers map[string]timesync.Sample
	fail    map[string]error
	asked   []string
	// hold, when set, blocks every query until it is closed.
	hold chan struct{}
	// queried receives each address asked.
	queried chan string
}

func newServers() *fakeServers {
	return &fakeServers{answers: map[string]timesync.Sample{}, fail: map[string]error{}, queried: make(chan string, 16)}
}

func (f *fakeServers) Query(ctx context.Context, addr string) (timesync.Sample, error) {
	f.mu.Lock()
	f.asked = append(f.asked, addr)
	hold, err, smp := f.hold, f.fail[addr], f.answers[addr]
	f.mu.Unlock()
	f.queried <- addr
	if hold != nil {
		<-hold
	}
	return smp, err
}

func (f *fakeServers) askedOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func TestSyncNow_StepsTheClockFromTheThresholdAndSlewsBelowIt(t *testing.T) {
	tests := map[string]struct {
		offset time.Duration
		step   bool
	}{
		"ahead by the threshold":       {timesync.StepThreshold, true},
		"behind by the threshold":      {-timesync.StepThreshold, true},
		"ahead by an hour":             {time.Hour, true},
		"ahead by just under":          {timesync.StepThreshold - time.Nanosecond, false},
		"behind by just under":         {-timesync.StepThreshold + time.Nanosecond, false},
		"in step":                      {0, false},
		"ahead by a few milliseconds":  {5 * time.Millisecond, false},
		"behind by a few milliseconds": {-5 * time.Millisecond, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clk, srv := newClock(), newServers()
			srv.answers["ntp:123"] = timesync.Sample{Offset: tt.offset, Delay: 20 * time.Millisecond}
			s := timesync.New(clk, srv)
			s.Configure([]string{"ntp:123"})

			res, err := s.SyncNow(context.Background())

			require.NoError(t, err)
			stepped, slewed := clk.moves()
			if tt.step {
				assert.Equal(t, []time.Duration{tt.offset}, stepped)
				assert.Empty(t, slewed)
				assert.Equal(t, timesync.Step, res.Correction)
			} else {
				assert.Equal(t, []time.Duration{tt.offset}, slewed)
				assert.Empty(t, stepped)
				assert.Equal(t, timesync.Slew, res.Correction)
			}
			assert.Equal(t, tt.offset, res.Offset, "the result keeps the offset measured before the correction")
			assert.Equal(t, "ntp:123", res.Server)
			assert.Equal(t, start, res.At)
		})
	}
}

func TestSyncNow_UsesTheFirstServerThatAnswers(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.fail["a:123"] = errors.New("no answer")
	srv.answers["b:123"] = timesync.Sample{Offset: time.Second}
	srv.answers["c:123"] = timesync.Sample{Offset: time.Minute}
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123", "b:123", "c:123"})

	res, err := s.SyncNow(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "b:123", res.Server)
	assert.Equal(t, []string{"a:123", "b:123"}, srv.askedOrder(), "the servers after the one that answered are not asked")
	stepped, _ := clk.moves()
	assert.Equal(t, []time.Duration{time.Second}, stepped)
}

func TestSyncNow_LeavesTheClockAloneAndReportsEachServerWhenNoneAnswers(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.fail["a:123"] = errors.New("first is down")
	srv.fail["b:123"] = errors.New("second is down")
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123", "b:123"})

	_, err := s.SyncNow(context.Background())

	require.Error(t, err)
	assert.ErrorContains(t, err, "a:123: first is down")
	assert.ErrorContains(t, err, "b:123: second is down")
	stepped, slewed := clk.moves()
	assert.Empty(t, stepped)
	assert.Empty(t, slewed)
	st := s.Status()
	assert.Nil(t, st.Last)
	assert.Equal(t, start, st.Failed)
	assert.Contains(t, st.Err, "first is down")
}

func TestSyncNow_FailsWithoutServers(t *testing.T) {
	s := timesync.New(newClock(), newServers())

	_, err := s.SyncNow(context.Background())

	assert.ErrorContains(t, err, "no time server")
}

func TestSyncNow_ReportsAClockItCannotSetWithoutAskingTheNextServer(t *testing.T) {
	clk, srv := newClock(), newServers()
	clk.err = errors.New("operation not permitted")
	srv.answers["a:123"] = timesync.Sample{Offset: time.Hour}
	srv.answers["b:123"] = timesync.Sample{Offset: time.Hour}
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123", "b:123"})

	_, err := s.SyncNow(context.Background())

	assert.ErrorContains(t, err, "operation not permitted")
	assert.Equal(t, []string{"a:123"}, srv.askedOrder())
	assert.Nil(t, s.Status().Last, "a correction that failed is not a synchronisation")
}

func TestSyncNow_StopsAskingWhenTheContextEnds(t *testing.T) {
	srv := newServers()
	srv.fail["a:123"] = errors.New("down")
	s := timesync.New(newClock(), srv)
	s.Configure([]string{"a:123", "b:123"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.SyncNow(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, srv.askedOrder(), "the bound has passed, so no server is asked")
}

func TestStatus_ReportsTheServersAndTheLastSynchronisation(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{Offset: 2 * time.Second, Delay: 30 * time.Millisecond}
	s := timesync.New(clk, srv)
	assert.Equal(t, timesync.Status{}, s.Status(), "a Syncer given nothing keeps no time")

	s.Configure([]string{"a:123"})
	_, err := s.SyncNow(context.Background())
	require.NoError(t, err)

	st := s.Status()
	assert.Equal(t, []string{"a:123"}, st.Servers)
	require.NotNil(t, st.Last)
	assert.Equal(t, timesync.Result{Server: "a:123", Offset: 2 * time.Second, Delay: 30 * time.Millisecond, Correction: timesync.Step, At: start}, *st.Last)
	assert.Zero(t, st.Failed)
	assert.Empty(t, st.Err)

	st.Servers[0] = "changed"
	assert.Equal(t, []string{"a:123"}, s.Status().Servers, "the status is a copy")
}

func TestStatus_ClearsTheFailureWhenALaterSynchronisationSucceeds(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.fail["a:123"] = errors.New("down")
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	_, err := s.SyncNow(context.Background())
	require.Error(t, err)

	srv.mu.Lock()
	delete(srv.fail, "a:123")
	srv.mu.Unlock()
	_, err = s.SyncNow(context.Background())

	require.NoError(t, err)
	st := s.Status()
	assert.Zero(t, st.Failed)
	assert.Empty(t, st.Err)
	assert.NotNil(t, st.Last)
}

// runSyncer runs s in the background and returns the function that stops it and waits.
func runSyncer(t *testing.T, s *timesync.Syncer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return when its context ended")
		}
	})
}

func nextTimer(t *testing.T, clk *fakeClock) timer {
	t.Helper()
	select {
	case tm := <-clk.timers:
		return tm
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Run waited on no timer")
		return timer{}
	}
}

func nextQuery(t *testing.T, srv *fakeServers) string {
	t.Helper()
	select {
	case addr := <-srv.queried:
		return addr
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no server was asked")
		return ""
	}
}

func TestRun_SynchronisesAtOnceThenEveryPollIntervalAfterASuccess(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{Offset: time.Millisecond}
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	runSyncer(t, s)

	assert.Equal(t, "a:123", nextQuery(t, srv))
	tm := nextTimer(t, clk)
	assert.Equal(t, timesync.PollInterval, tm.wait)

	clk.advance(timesync.PollInterval)
	tm.fire <- clk.Now()
	assert.Equal(t, "a:123", nextQuery(t, srv), "the poll interval passed")
	assert.Equal(t, timesync.PollInterval, nextTimer(t, clk).wait)
}

func TestRun_TriesAgainAfterTheRetryIntervalWhenNoServerAnswered(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.fail["a:123"] = errors.New("down")
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	runSyncer(t, s)

	assert.Equal(t, "a:123", nextQuery(t, srv))
	tm := nextTimer(t, clk)
	assert.Equal(t, timesync.RetryInterval, tm.wait)

	clk.advance(timesync.RetryInterval)
	tm.fire <- clk.Now()
	assert.Equal(t, "a:123", nextQuery(t, srv))
}

func TestRun_WaitsWhileNoServerIsConfiguredAndSynchronisesWhenOneIs(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{}
	s := timesync.New(clk, srv)
	runSyncer(t, s)

	select {
	case addr := <-srv.queried:
		require.FailNow(t, "a server was asked with none configured: "+addr)
	case <-time.After(100 * time.Millisecond):
	}

	s.Configure([]string{"a:123"})
	assert.Equal(t, "a:123", nextQuery(t, srv))
}

func TestRun_FollowsTheServersConfiguredLastAtOnce(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{}
	srv.answers["b:123"] = timesync.Sample{}
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	runSyncer(t, s)
	assert.Equal(t, "a:123", nextQuery(t, srv))
	nextTimer(t, clk)

	s.Configure([]string{"b:123"})

	assert.Equal(t, "b:123", nextQuery(t, srv), "the new servers are asked without waiting out the poll interval")
	assert.Equal(t, []string{"b:123"}, s.Status().Servers)
}

func TestRun_AsksNoServerOnceTheServersAreRemoved(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{}
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	runSyncer(t, s)
	assert.Equal(t, "a:123", nextQuery(t, srv))
	tm := nextTimer(t, clk)

	s.Configure(nil)
	clk.advance(timesync.PollInterval)
	tm.fire <- clk.Now()

	select {
	case addr := <-srv.queried:
		require.FailNow(t, "a server was asked after the servers were removed: "+addr)
	case <-time.After(200 * time.Millisecond):
	}
	assert.Empty(t, s.Status().Servers)
}

func TestSyncNow_DoesNotDeferTheSynchronisationAConfigureAskedForDuringIt(t *testing.T) {
	clk, srv := newClock(), newServers()
	srv.answers["a:123"] = timesync.Sample{}
	srv.answers["b:123"] = timesync.Sample{}
	srv.hold = make(chan struct{})
	s := timesync.New(clk, srv)
	s.Configure([]string{"a:123"})
	done := make(chan error, 1)
	go func() {
		_, err := s.SyncNow(context.Background())
		done <- err
	}()
	assert.Equal(t, "a:123", nextQuery(t, srv))

	s.Configure([]string{"b:123"})
	close(srv.hold)
	require.NoError(t, <-done)

	runSyncer(t, s)
	assert.Equal(t, "b:123", nextQuery(t, srv), "the attempt that was in flight when Configure ran must not push the next one a poll interval away")
}
