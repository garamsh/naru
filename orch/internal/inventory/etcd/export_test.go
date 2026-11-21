package etcd

import "time"

// MaxEvents is the bound a store keeps by default.
const MaxEvents = maxEvents

// KeepEvents makes s keep n events, so a test can fill the log past its bound quickly.
func KeepEvents(s *Store, n int) { s.keepEvents = n }

// MaxEventAge is how old an event may be before the store drops it.
const MaxEventAge = maxEventAge

// SetClock makes s date the age bound of the event log by now.
func SetClock(s *Store, now func() time.Time) { s.now = now }
