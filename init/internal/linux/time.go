package linux

import (
	"context"
	"fmt"
	"log/slog"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/timesync"
)

// adjOffsetSingleshot is the adjtimex mode that slews the clock by Offset and replaces any
// slew still running, as adjtime(3) does.
const adjOffsetSingleshot = 0x8001

// SystemClock is the clock of the machine, which it sets.
type SystemClock struct{}

// Now returns the time.
func (SystemClock) Now() time.Time { return time.Now() }

// After waits for d and then sends the time on the returned channel.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Step sets the clock offset ahead of what it reads now.
func (SystemClock) Step(offset time.Duration) error {
	tv := syscall.NsecToTimeval(time.Now().Add(offset).UnixNano())
	if err := syscall.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	return nil
}

// Slew has the kernel bring the clock in line by offset, at most 500 microseconds per second.
func (SystemClock) Slew(offset time.Duration) error {
	tx := syscall.Timex{Modes: adjOffsetSingleshot, Offset: int64(offset / time.Microsecond)}
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return fmt.Errorf("adjtimex: %w", err)
	}
	return nil
}

// SyncTime points the clock at the servers, each a host:port, and synchronises with the first
// that answers within timesync.BootBound. It returns an error when none does; the clock is then
// as it was.
func (h *Host) SyncTime(servers []string) (timesync.Result, error) {
	h.time.Configure(servers)
	ctx, cancel := context.WithTimeout(context.Background(), timesync.BootBound)
	defer cancel()
	return h.time.SyncNow(ctx)
}

// ConfigureTime replaces the servers the clock follows, none to stop following any, and has the
// clock synchronise in the background at once.
func (h *Host) ConfigureTime(servers []string) {
	h.time.Configure(servers)
}

// RunTime keeps the clock in step with the servers until ctx ends, and logs each outcome.
func (h *Host) RunTime(ctx context.Context, log *slog.Logger) {
	h.time.Run(ctx, log)
}

// TimeStatus returns the servers the clock follows and what the last synchronisation did.
func (h *Host) TimeStatus() timesync.Status {
	return h.time.Status()
}
