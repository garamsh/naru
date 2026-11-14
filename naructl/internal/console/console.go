// Package console is the web console of naructl: a server on the operator's workstation that shows
// what orch holds and signs an admission with the operator's key, which never leaves the process.
package console

import "errors"

// ErrCodeMismatch is returned when the pairing code an operator typed is not the code of the pending
// host's key.
var ErrCodeMismatch = errors.New("the pairing code does not match the pending host's key")

// ErrNoPending is returned when orch lists no pending host of the name an operator admits.
var ErrNoPending = errors.New("no such pending host")

// MicroVM is a microVM on a host.
type MicroVM struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Group string `json:"group"`
	// Image is the image digest of an image-form microVM, or the boot set of an OS-form one.
	Image string `json:"image"`
	// Network is the virtual network the microVM is on, or empty.
	Network string `json:"network"`
}

// Group is a group of microVMs orch keeps at its count.
type Group struct {
	Name     string `json:"name"`
	Count    int    `json:"count"`
	Ready    int    `json:"ready"`
	Stateful bool   `json:"stateful"`
}
