package hostapi

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// The bounds of a guest call: an exec runs at most MaxExecSeconds, DefaultExecSeconds when its
// request names none, and an exec's output, stdout and stderr together, or a guest file is at most
// MaxGuestBytes (413 above it).
const (
	MaxExecSeconds     = 600
	DefaultExecSeconds = 600
	MaxGuestBytes      = 16 << 20
)

// The codes of a guest call's refusals.
const (
	// CodeNotRunning: 409, an exec or a file call to a microVM that is not running.
	CodeNotRunning = "not-running"
	// CodeFileNotFound: 404, a file call to a running microVM whose guest has no file at the path.
	CodeFileNotFound = "file-not-found"
)

// ExecRequest is the body of POST /v1/microvms/{name}/exec: the command, run in the guest without
// input, and the seconds it may run before it is ended with SIGKILL.
type ExecRequest struct {
	Command        []string `json:"command"`
	TimeoutSeconds uint32   `json:"timeoutSeconds,omitempty"`
}

// Check refuses an empty command and a timeout above MaxExecSeconds, naming the field.
func (r ExecRequest) Check() (field string, err error) {
	switch {
	case len(r.Command) == 0 || r.Command[0] == "":
		return "command", errors.New("command is empty")
	case r.TimeoutSeconds > MaxExecSeconds:
		return "timeoutSeconds", fmt.Errorf("timeoutSeconds is %d; at most %d", r.TimeoutSeconds, MaxExecSeconds)
	}
	return "", nil
}

// Timeout is the seconds the command may run: TimeoutSeconds, DefaultExecSeconds when it is 0.
func (r ExecRequest) Timeout() uint32 {
	if r.TimeoutSeconds == 0 {
		return DefaultExecSeconds
	}
	return r.TimeoutSeconds
}

// ExecResult is an exec that ran, whole. A command a signal ended has Signal, its number as on
// Linux, and ExitCode 128 plus it, as a shell gives; TimedOut is a command ended with SIGKILL at
// its timeout. A command that did not start has ExitCode 127 and the reason in Stderr.
type ExecResult struct {
	ExitCode int32  `json:"exitCode"`
	Signal   int32  `json:"signal,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
}

// CheckGuestPath refuses a guest path that is not absolute and clean, is "/", or is under /proc,
// /sys, /dev or /run/mvmd, as mvmd refuses it.
func CheckGuestPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("%q is not an absolute, clean path below /", p)
	}
	for _, d := range []string{"/proc", "/sys", "/dev", "/run/mvmd"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return fmt.Errorf("%q is under %s", p, d)
		}
	}
	return nil
}
