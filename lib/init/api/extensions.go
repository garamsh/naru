package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/garamsh/naru/lib/init/extension"
)

// Extensions is the Node's store of system extension images.
type Extensions interface {
	// Put stores the image read from body, whose SHA-256 must be digest.
	Put(body io.Reader, digest string) (extension.Entry, error)
	// Delete removes the stored image unless it is mounted.
	Delete(digest string) error
	// List returns every stored image and whether it is mounted.
	List() ([]extension.Entry, error)
}

// extensions returns the store, and answers 501 and false on a Node that has none.
func (s *Server) extensions(w http.ResponseWriter) (Extensions, bool) {
	if s.deps.Extensions == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "this Node keeps no system extensions"})
		return nil, false
	}
	return s.deps.Extensions, true
}

func (s *Server) getExtensions(w http.ResponseWriter, _ *http.Request) {
	store, ok := s.extensions(w)
	if !ok {
		return
	}
	list, err := store.List()
	if err != nil {
		s.deps.Log.Error("list the stored extensions", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the stored extensions cannot be listed; see the Node's log"})
		return
	}
	if list == nil {
		list = []extension.Entry{}
	}
	writeJSON(w, http.StatusOK, struct {
		Extensions []extension.Entry `json:"extensions"`
	}{list})
}

// putExtension stores the image in the request body under the SHA-256 in the path. The digest
// is checked against the bytes received, so a body that is not the image the path names is 400.
func (s *Server) putExtension(w http.ResponseWriter, r *http.Request) {
	store, ok := s.extensions(w)
	if !ok {
		return
	}
	entry, err := store.Put(r.Body, r.PathValue("sha256"))
	if err != nil {
		s.extensionRefused(w, r, "store an extension", err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) deleteExtension(w http.ResponseWriter, r *http.Request) {
	store, ok := s.extensions(w)
	if !ok {
		return
	}
	if err := store.Delete(r.PathValue("sha256")); err != nil {
		s.extensionRefused(w, r, "remove an extension", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// extensionRefused answers a request the store refused: 400 for an image the caller got wrong, 413
// too large, 404 not stored, 409 mounted, and 500 for anything else, with the reason left to the log.
func (s *Server) extensionRefused(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, extension.ErrDigest), errors.Is(err, extension.ErrImage), errors.Is(err, extension.ErrTooLarge):
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, extension.MaxBytes))
	}
	switch {
	case errors.Is(err, extension.ErrDigest), errors.Is(err, extension.ErrImage):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	case errors.Is(err, extension.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: err.Error()})
	case errors.Is(err, extension.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error()})
	case errors.Is(err, extension.ErrMounted):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	default:
		s.deps.Log.Error(what, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "could not " + what + "; see the Node's log"})
	}
}
