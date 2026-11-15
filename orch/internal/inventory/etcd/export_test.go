package etcd

// MaxEvents is the bound a store keeps by default.
const MaxEvents = maxEvents

// KeepEvents makes s keep n events, so a test can fill the log past its bound quickly.
func KeepEvents(s *Store, n int) { s.keepEvents = n }
