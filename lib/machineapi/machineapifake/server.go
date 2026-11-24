package machineapifake

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/garamsh/naru/lib/machineapi"
)

// CallerRole is the OrganizationalUnit of a caller certificate (ADR 0108 item 4).
const CallerRole = "caller"

// maxBody bounds a request body: a spec's files hold at most 1 MiB, base64 in JSON.
const maxBody = 4 << 20

// maxFile bounds the content of a guest file a PUT writes.
const maxFile = 16 << 20

func (f *Fake) routes() {
	m := http.NewServeMux()
	p := machineapi.PathPrefix
	m.HandleFunc("PUT "+p+"machines/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		var spec machineapi.Spec
		if err := decode(r, &spec); err != nil {
			return 0, nil, err
		}
		mc, created, err := f.create(c, r.PathValue("name"), spec)
		return createdOr(created), mc, err
	}))
	m.HandleFunc("GET "+p+"machines", f.handle(func(c string, _ *http.Request) (int, any, error) {
		return http.StatusOK, f.machines(c), nil
	}))
	m.HandleFunc("GET "+p+"machines/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		mc, err := f.machine(c, r.PathValue("name"))
		return http.StatusOK, mc, err
	}))
	m.HandleFunc("POST "+p+"machines/{name}/start", f.handle(func(c string, r *http.Request) (int, any, error) {
		mc, err := f.setState(c, r.PathValue("name"), machineapi.StateRunning)
		return http.StatusOK, mc, err
	}))
	m.HandleFunc("POST "+p+"machines/{name}/stop", f.handle(func(c string, r *http.Request) (int, any, error) {
		var s machineapi.StopRequest
		if err := decode(r, &s); err != nil {
			return 0, nil, err
		}
		mc, err := f.setState(c, r.PathValue("name"), machineapi.StateStopped)
		return http.StatusOK, mc, err
	}))
	m.HandleFunc("DELETE "+p+"machines/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		mc, err := f.deleteMachine(c, r.PathValue("name"))
		return http.StatusAccepted, mc, err
	}))
	m.HandleFunc("POST "+p+"machines/{name}/exec", f.handle(func(c string, r *http.Request) (int, any, error) {
		var e machineapi.ExecRequest
		if err := decode(r, &e); err != nil {
			return 0, nil, err
		}
		out, err := f.exec(c, r.PathValue("name"), e)
		return http.StatusOK, out, err
	}))
	m.HandleFunc("GET "+p+"machines/{name}/files", f.handle(func(c string, r *http.Request) (int, any, error) {
		b, err := f.readFile(c, r.PathValue("name"), r.URL.Query().Get("path"))
		return http.StatusOK, raw(b), err
	}))
	m.HandleFunc("PUT "+p+"machines/{name}/files", f.handle(func(c string, r *http.Request) (int, any, error) {
		b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxFile))
		if err != nil {
			return 0, nil, refuse(http.StatusRequestEntityTooLarge, machineapi.CodeInvalid, "the file is larger than %d bytes", maxFile)
		}
		return http.StatusNoContent, nil, f.writeFile(c, r.PathValue("name"), r.URL.Query().Get("path"), b)
	}))
	m.HandleFunc("GET "+p+"events", f.handle(func(c string, r *http.Request) (int, any, error) {
		page, err := f.events(c, r.URL.Query().Get("since"))
		return http.StatusOK, page, err
	}))
	m.HandleFunc("PUT "+p+"snapshots/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		var s machineapi.SnapshotRequest
		if err := decode(r, &s); err != nil {
			return 0, nil, err
		}
		snap, created, err := f.putSnapshot(c, r.PathValue("name"), s)
		return createdOr(created), snap, err
	}))
	m.HandleFunc("GET "+p+"snapshots", f.handle(func(c string, _ *http.Request) (int, any, error) {
		return http.StatusOK, f.snapshots(c), nil
	}))
	m.HandleFunc("GET "+p+"snapshots/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		s, err := f.snapshot(c, r.PathValue("name"))
		return http.StatusOK, s, err
	}))
	m.HandleFunc("DELETE "+p+"snapshots/{name}", f.handle(func(c string, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, f.deleteSnapshot(c, r.PathValue("name"))
	}))
	m.HandleFunc("PUT "+p+"aliases/{hostname}", f.handle(func(c string, r *http.Request) (int, any, error) {
		var a machineapi.AliasRequest
		if err := decode(r, &a); err != nil {
			return 0, nil, err
		}
		al, created, err := f.putAlias(c, r.PathValue("hostname"), a)
		return createdOr(created), al, err
	}))
	m.HandleFunc("GET "+p+"aliases/{hostname}", f.handle(func(c string, r *http.Request) (int, any, error) {
		al, err := f.alias(c, r.PathValue("hostname"))
		return http.StatusOK, al, err
	}))
	m.HandleFunc("DELETE "+p+"aliases/{hostname}", f.handle(func(c string, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, f.deleteAlias(c, r.PathValue("hostname"))
	}))
	f.mux = m
}

// ServeHTTP answers the Machine API to a caller certificate and 403 to any other.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.mux.ServeHTTP(w, r) }

// raw is a body answered as it is, not as JSON.
type raw []byte

func createdOr(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

// handle answers op's result to the caller the request's certificate names.
func (f *Fake) handle(op func(caller string, r *http.Request) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, err := callerOf(r)
		status, body := 0, any(nil)
		if err == nil {
			status, body, err = op(caller, r)
		}
		if err != nil {
			status, eb := errorOf(err)
			writeJSON(w, status, eb)
			return
		}
		switch b := body.(type) {
		case nil:
			w.WriteHeader(status)
		case raw:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(status)
			_, _ = w.Write(b)
		default:
			writeJSON(w, status, b)
		}
	}
}

// callerOf returns the caller the verified client certificate names. The TLS configuration
// verifies the chain; a certificate of another role is refused.
func callerOf(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", refuse(http.StatusForbidden, machineapi.CodeForbidden, "a caller certificate is required")
	}
	leaf := r.TLS.PeerCertificates[0]
	if !slices.Equal(leaf.Subject.OrganizationalUnit, []string{CallerRole}) || machineapi.CheckName(leaf.Subject.CommonName) != nil {
		return "", refuse(http.StatusForbidden, machineapi.CodeForbidden, "the certificate is not of the role %s", CallerRole)
	}
	return leaf.Subject.CommonName, nil
}

// decode reads the JSON body into v, refusing a body that is not that JSON.
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return refuse(http.StatusBadRequest, machineapi.CodeInvalid, "the body is not the operation's JSON: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
