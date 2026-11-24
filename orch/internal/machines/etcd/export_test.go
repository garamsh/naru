package etcd

import "time"

// KeepEvents makes s keep n events in each caller's log, so a test fills a log past its bound quickly.
func KeepEvents(s *Store, n int) { s.keepEvents = n }

// MaxEventAge is how old an event may be before the store drops it.
const MaxEventAge = maxEventAge

// SetClock makes s date the age bound of the logs by now.
func SetClock(s *Store, now func() time.Time) { s.now = now }
