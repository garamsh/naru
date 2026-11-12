package apply

import (
	"fmt"
	"strings"
)

// StaleError is returned when the request names a revision other than the
// stored one. It maps to 409.
type StaleError struct {
	// Stored is the revision the Node holds.
	Stored int
	// Requested is the revision the request named.
	Requested int
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("the request names revision %d and the Node is at revision %d", e.Requested, e.Stored)
}

// InvalidError is returned when the request is malformed, its result does not
// validate, or its result changes a field the mode does not allow. It maps to
// 422. Nothing was stored and nothing was done.
type InvalidError struct {
	// Fields are the fields the refusal concerns, as paths from the document
	// root such as spec.services[0].binary. They are never empty.
	Fields  []string
	Message string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%s: %s", strings.Join(e.Fields, ", "), e.Message)
}

// FailedError is returned when a live action fails after the new revision was
// stored. It maps to 500. The stored revision stays and nothing is undone.
type FailedError struct {
	// Revision is the revision that was stored.
	Revision int
	// Done are the actions that took effect, the store first.
	Done []Action
	// Failed is the action that failed.
	Failed Action
	Err    error
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("%s %s failed after revision %d was stored: %v", e.Failed.Action, e.Failed.Target, e.Revision, e.Err)
}

func (e *FailedError) Unwrap() error { return e.Err }

// StoreError is returned when the store cannot be written. It maps to 500.
// Nothing was stored and nothing was done.
type StoreError struct{ Err error }

func (e *StoreError) Error() string { return "store the configuration: " + e.Err.Error() }

func (e *StoreError) Unwrap() error { return e.Err }
