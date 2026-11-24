package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// sigkill is the signal that ends an exec at its timeout or above its output bound.
const sigkill = 9

// errTooLarge is an exec's output or a guest file above hostapi.MaxGuestBytes.
var errTooLarge = fmt.Errorf("larger than %d bytes", hostapi.MaxGuestBytes)

// guestRead reports whether the request reads a guest's file, which may hold its secrets, so it
// needs the provisioner role as a change does.
func guestRead(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/microvms/") && strings.HasSuffix(r.URL.Path, "/files")
}

// running returns the name of the request's microVM when it runs. An absent one is answered 404,
// and one in another state 409 not-running.
func (s *Server) running(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	m, err := s.deps.Runtime.InspectMachine(ctx, name)
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	if m.State != mvmd.StateRunning {
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: fmt.Sprintf("the microVM %s is %s, not running", name, m.State), Code: hostapi.CodeNotRunning})
		return "", false
	}
	return name, true
}

// failGuest answers a guest call's error: a Machine that stopped meanwhile is 409 not-running.
func (s *Server) failGuest(w http.ResponseWriter, err error) {
	if errors.Is(err, mvmd.ErrPrecondition) {
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: err.Error(), Code: hostapi.CodeNotRunning})
		return
	}
	s.fail(w, err)
}

func tooLarge(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusRequestEntityTooLarge, hostapi.ErrorBody{Error: what + " is " + errTooLarge.Error()})
}

// output is an exec's output as it is read, bounded by hostapi.MaxGuestBytes.
type output struct {
	stdout, stderr []byte
	// next is the offset after the last chunk taken, which a read after a kill resumes from.
	next uint64
}

func (o *output) add(c mvmd.Chunk) error {
	if len(o.stdout)+len(o.stderr)+len(c.Data) > hostapi.MaxGuestBytes {
		return errTooLarge
	}
	if c.Stderr {
		o.stderr = append(o.stderr, c.Data...)
	} else {
		o.stdout = append(o.stdout, c.Data...)
	}
	o.next = c.Offset + uint64(len(c.Data))
	return nil
}

// execMicroVM runs a command in the running microVM's guest and answers its result once it ended.
// A command still running at its timeout is ended with SIGKILL and answered as timed out; one whose
// output passes hostapi.MaxGuestBytes is ended so and answered 413; one the request's end leaves
// running is ended so too.
func (s *Server) execMicroVM(w http.ResponseWriter, r *http.Request) {
	var req hostapi.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	if field, err := req.Check(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: err.Error(), Fields: []string{field}})
		return
	}
	name, ok := s.running(r.Context(), w, r)
	if !ok {
		return
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	execID := "naru-" + hex.EncodeToString(id[:])
	rt := s.deps.Runtime
	rec, err := rt.Exec(r.Context(), name, execID, req.Command)
	if err != nil {
		s.failGuest(w, err)
		return
	}
	if rec.Status == mvmd.ExecNotStarted {
		writeJSON(w, http.StatusOK, hostapi.ExecResult{ExitCode: 127, Stdout: []byte{}, Stderr: []byte(rec.Reason)})
		return
	}
	var out output
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.Timeout())*time.Second)
	rec, err = rt.ReadExecOutput(ctx, name, execID, 0, out.add)
	cancel()
	timedOut := false
	if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
		// The timeout, not the request's end: the command is ended, and its record read.
		s.kill(r.Context(), name, execID)
		timedOut = true
		kctx, cancel := cleanupContext(r.Context())
		rec, err = rt.ReadExecOutput(kctx, name, execID, out.next, out.add)
		cancel()
	}
	switch {
	case errors.Is(err, errTooLarge):
		s.kill(r.Context(), name, execID)
		tooLarge(w, "the output")
		return
	case err != nil:
		if r.Context().Err() != nil {
			s.kill(r.Context(), name, execID)
		}
		s.failGuest(w, err)
		return
	case rec.Status == mvmd.ExecCutShort:
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "the microVM stopped while the command ran: " + rec.Reason, Code: hostapi.CodeNotRunning})
		return
	}
	res := hostapi.ExecResult{ExitCode: rec.ExitCode, TimedOut: timedOut, Stdout: out.stdout, Stderr: out.stderr}
	if rec.TermSignal != 0 {
		res.Signal, res.ExitCode = rec.TermSignal, 128+rec.TermSignal
	}
	if res.Stdout == nil {
		res.Stdout = []byte{}
	}
	if res.Stderr == nil {
		res.Stderr = []byte{}
	}
	writeJSON(w, http.StatusOK, res)
}

// kill ends the exec with SIGKILL, on a context of its own, since the request's may be what ended.
// A command that has ended already is no failure.
func (s *Server) kill(parent context.Context, name, execID string) {
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	if err := s.deps.Runtime.SignalExec(ctx, name, execID, sigkill); err != nil && !errors.Is(err, mvmd.ErrPrecondition) {
		s.deps.Log.Error("could not end an exec", "microvm", name, "exec", execID, "err", err.Error())
	}
}

// guestPath returns the request's path query when it is one mvmd takes, and answers 422 otherwise.
func guestPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if err := hostapi.CheckGuestPath(p); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: "path: " + err.Error(), Fields: []string{"path"}})
		return "", false
	}
	return p, true
}

// readFile answers the content of one file of the running microVM's guest. An absent file is 404
// file-not-found, and one above hostapi.MaxGuestBytes 413.
func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	p, ok := guestPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	name, ok := s.running(ctx, w, r)
	if !ok {
		return
	}
	var content bytes.Buffer
	err := s.deps.Runtime.ReadFile(ctx, name, p, func(b []byte) error {
		if content.Len()+len(b) > hostapi.MaxGuestBytes {
			return errTooLarge
		}
		content.Write(b)
		return nil
	})
	switch {
	case errors.Is(err, errTooLarge):
		tooLarge(w, "the file")
	case errors.Is(err, mvmd.ErrNotFound):
		writeJSON(w, http.StatusNotFound, hostapi.ErrorBody{Error: fmt.Sprintf("no file %s in the microVM %s", p, name), Code: hostapi.CodeFileNotFound})
	case err != nil:
		s.failGuest(w, err)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content.Bytes())
	}
}

// writeFile replaces one file of the running microVM's guest with the request's body, whole. A body
// above hostapi.MaxGuestBytes is 413 and reaches no guest.
func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	p, ok := guestPath(w, r)
	if !ok {
		return
	}
	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hostapi.MaxGuestBytes))
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		tooLarge(w, "the file")
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body could not be read"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	name, ok := s.running(ctx, w, r)
	if !ok {
		return
	}
	if err := s.deps.Runtime.WriteFile(ctx, name, p, bytes.NewReader(content)); err != nil {
		s.failGuest(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
