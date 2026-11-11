package service

import "time"

// Clock is the time the manager measures runs and delays against.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the Clock of the machine.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// After waits for d and then sends the time on the returned channel.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
