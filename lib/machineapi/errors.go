package machineapi

import "fmt"

// Code names a refusal, so a caller branches on it and not on the message.
type Code string

// The codes, each with the status it comes with. A client keeps a code it does not know as the
// string it is.
const (
	// CodeInvalid: 400 for a body that is not the operation's JSON, 422 for a field it refuses.
	CodeInvalid Code = "invalid"
	// CodeForbidden: 403, a certificate of a role other than caller.
	CodeForbidden Code = "forbidden"
	// CodeNotFound: 404, nothing of the caller's at the path. Another caller's Machine, snapshot or
	// alias is answered so.
	CodeNotFound Code = "not-found"
	// CodeConflict: 409, the name holds another spec, a snapshot of another Machine, or an alias of
	// another Machine or service.
	CodeConflict Code = "conflict"
	// CodeDeleting: 409, the name is held by a Machine being deleted until its volumes are gone.
	CodeDeleting Code = "deleting"
	// CodeNotRunning: 409, an exec or a file call to a Machine that is not running.
	CodeNotRunning Code = "not-running"
	// CodeCursorExpired: 410, a cursor older than the event log keeps: list again.
	CodeCursorExpired Code = "cursor-expired"
	// CodeMachineNotFound: 422, a snapshot or an alias names a Machine the caller has not.
	CodeMachineNotFound Code = "machine-not-found"
	// CodeSnapshotNotFound: 422, a restore names a snapshot the caller has not.
	CodeSnapshotNotFound Code = "snapshot-not-found"
	// CodeSnapshotMismatch: 422, a restore's volumes do not match the snapshot's by name and size.
	CodeSnapshotMismatch Code = "snapshot-mismatch"
	// CodeAliasTaken: 409, another caller holds the hostname.
	CodeAliasTaken Code = "alias-taken"
	// CodeAliasDomain: 403, a hostname outside the zone in no domain the operator granted the
	// caller.
	CodeAliasDomain Code = "alias-domain-not-granted"
	// CodeInternal: 500, a failure of the server's own.
	CodeInternal Code = "internal"
)

// ErrorBody is the body of every refusal and failure.
type ErrorBody struct {
	Error string `json:"error"`
	Code  Code   `json:"code"`
}

// Error is a refusal or failure the Machine API answered. Read it with errors.As.
type Error struct {
	Status  int
	Message string
	Code    Code
}

func (e *Error) Error() string {
	return fmt.Sprintf("machine API %d %s: %s", e.Status, e.Code, e.Message)
}
